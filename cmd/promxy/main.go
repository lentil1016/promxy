package main

import (
	"context"
	"encoding/json"
	"io"

	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "net/http/pprof"

	"crypto/md5"

	kitlog "github.com/go-kit/kit/log"
	"github.com/jacksontj/promxy/config"
	"github.com/jacksontj/promxy/logging"
	"github.com/jacksontj/promxy/proxystorage"
	"github.com/jessevdk/go-flags"
	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/route"
	"github.com/prometheus/common/version"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	sd_config "github.com/prometheus/prometheus/discovery/config"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/scrape"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/strutil"
	"github.com/prometheus/prometheus/web"
	"github.com/sirupsen/logrus"
)

var (
	reloadTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "process_reload_time_seconds",
		Help: "Last reload (SIGHUP) time of the process since unix epoch in seconds.",
	})
)

type CLIOpts struct {
	BindAddr   string `long:"bind-addr" description:"address for promxy to listen on" default:":8082"`
	ConfigFile string `long:"config" description:"path to the config file" required:"true"`
	LogLevel   string `long:"log-level" description:"Log level" default:"info"`

	ExternalURL string `long:"web.external-url" description:"The URL under which Prometheus is externally reachable (for example, if Prometheus is served via a reverse proxy). Used for generating relative and absolute links back to Prometheus itself. If the URL has a path portion, it will be used to prefix all HTTP endpoints served by Prometheus. If omitted, relevant URL components will be derived automatically."`

	QueryTimeout        time.Duration `long:"query.timeout" description:"Maximum time a query may take before being aborted." default:"2m"`
	QueryMaxConcurrency int           `long:"query.max-concurrency" description:"Maximum number of queries executed concurrently." default:"1000"`

	NotificationQueueCapacity int    `long:"alertmanager.notification-queue-capacity" description:"The capacity of the queue for pending alert manager notifications." default:"10000"`
	AccessLogDestination      string `long:"access-log-destination" description:"where to log access logs, options (none, stderr, stdout)" default:"stdout"`
}

func (c *CLIOpts) ToFlags() map[string]string {
	tmp := make(map[string]string)
	// TODO: better
	b, _ := json.Marshal(opts)
	json.Unmarshal(b, &tmp)
	return tmp
}

var opts CLIOpts

func reloadConfig(rls ...proxyconfig.Reloadable) error {
	cfg, err := proxyconfig.ConfigFromFile(opts.ConfigFile)
	if err != nil {
		return fmt.Errorf("Error loading cfg: %v", err)
	}

	failed := false
	for _, rl := range rls {
		if err := rl.ApplyConfig(cfg); err != nil {
			logrus.Errorf("Failed to apply configuration: %v", err)
			failed = true
		}
	}

	if failed {
		return fmt.Errorf("One or more errors occurred while applying new configuration")
	}
	reloadTime.Set(float64(time.Now().Unix()))
	return nil
}

func main() {
	// Wait for reload or termination signals. Start the handler for SIGHUP as
	// early as possible, but ignore it until we are ready to handle reloading
	// our config.
	sigs := make(chan os.Signal)
	defer close(sigs)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	prometheus.MustRegister(reloadTime)

	reloadables := make([]proxyconfig.Reloadable, 0)

	parser := flags.NewParser(&opts, flags.Default)
	if _, err := parser.Parse(); err != nil {
		// If the error was from the parser, then we can simply return
		// as Parse() prints the error already
		if _, ok := err.(*flags.Error); ok {
			os.Exit(1)
		}
		logrus.Fatalf("Error parsing flags: %v", err)
	}

	// Use log level
	level := logrus.InfoLevel
	switch strings.ToLower(opts.LogLevel) {
	case "panic":
		level = logrus.PanicLevel
	case "fatal":
		level = logrus.FatalLevel
	case "error":
		level = logrus.ErrorLevel
	case "warn":
		level = logrus.WarnLevel
	case "info":
		level = logrus.InfoLevel
	case "debug":
		level = logrus.DebugLevel
	default:
		logrus.Fatalf("Unknown log level: %s", opts.LogLevel)
	}
	logrus.SetLevel(level)

	// Set the log format to have a reasonable timestamp
	formatter := &logrus.TextFormatter{
		FullTimestamp: true,
	}
	logrus.SetFormatter(formatter)

	// Create base context for this daemon
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Reload ready -- channel to close once we are ready to start reloaders
	reloadReady := make(chan struct{}, 0)

	// Create the proxy storag
	var proxyStorage storage.Storage

	ps, err := proxystorage.NewProxyStorage()
	if err != nil {
		logrus.Fatalf("Error creating proxy: %v", err)
	}
	reloadables = append(reloadables, ps)
	proxyStorage = ps

	engine := promql.NewEngine(nil, prometheus.DefaultRegisterer, opts.QueryMaxConcurrency, opts.QueryTimeout)
	engine.NodeReplacer = ps.NodeReplacer

	// TODO: rename
	externalUrl, err := computeExternalURL(opts.ExternalURL, opts.BindAddr)
	if err != nil {
		logrus.Fatalf("Unable to parse external URL %s", "tmp")
	}

	// Alert notifier
	lvl := promlog.AllowedLevel{}
	if err := lvl.Set("info"); err != nil {
		panic(err)
	}
	logger := promlog.New(lvl)

	notifierManager := notifier.NewManager(
		&notifier.Options{
			Registerer:    prometheus.DefaultRegisterer,
			QueueCapacity: opts.NotificationQueueCapacity,
		},
		kitlog.With(logger, "component", "notifier"),
	)
	reloadables = append(reloadables, proxyconfig.WrapPromReloadable(notifierManager))

	discoveryManagerNotify := discovery.NewManager(ctx, kitlog.With(logger, "component", "discovery manager notify"))
	reloadables = append(reloadables,
		proxyconfig.WrapPromReloadable(&proxyconfig.ApplyConfigFunc{func(cfg *config.Config) error {
			c := make(map[string]sd_config.ServiceDiscoveryConfig)
			for _, v := range cfg.AlertingConfig.AlertmanagerConfigs {
				// AlertmanagerConfigs doesn't hold an unique identifier so we use the config hash as the identifier.
				b, err := json.Marshal(v)
				if err != nil {
					return err
				}
				c[fmt.Sprintf("%x", md5.Sum(b))] = v.ServiceDiscoveryConfig
			}
			return discoveryManagerNotify.ApplyConfig(c)
		}}),
	)

	go func() {
		if err := discoveryManagerNotify.Run(); err != nil {
			logrus.Errorf("Error running Notify discovery manager: %v", err)
		} else {
			logrus.Infof("Notify discovery manager stopped")
		}
	}()
	go func() {
		<-reloadReady
		notifierManager.Run(discoveryManagerNotify.SyncCh())
		logrus.Infof("Notifier manager stopped")
	}()

	ruleManager := rules.NewManager(&rules.ManagerOptions{
		Context:     ctx,         // base context for all background tasks
		ExternalURL: externalUrl, // URL listed as URL for "who fired this alert"
		QueryFunc:   rules.EngineQueryFunc(engine, proxyStorage),
		NotifyFunc:  sendAlerts(notifierManager, externalUrl.String()),
		Appendable:  proxyStorage,
		Logger:      logger,
	})
	go ruleManager.Run()

	reloadables = append(reloadables, proxyconfig.WrapPromReloadable(&proxyconfig.ApplyConfigFunc{func(cfg *config.Config) error {
		// Get all rule files matching the configuration oaths.
		var files []string
		for _, pat := range cfg.RuleFiles {
			fs, err := filepath.Glob(pat)
			if err != nil {
				// The only error can be a bad pattern.
				return fmt.Errorf("error retrieving rule files for %s: %s", pat, err)
			}
			files = append(files, fs...)
		}
		return ruleManager.Update(time.Duration(cfg.GlobalConfig.EvaluationInterval), files)
	}}))

	// We need an empty scrape manager, simply to make the API not panic and error out
	scrapeManager := scrape.NewManager(kitlog.With(logger, "component", "scrape manager"), nil)

	// TODO: separate package?
	webOptions := &web.Options{
		Context:       ctx,
		Storage:       proxyStorage,
		QueryEngine:   engine,
		ScrapeManager: scrapeManager,
		RuleManager:   ruleManager,
		Notifier:      notifierManager,

		Flags:       opts.ToFlags(),
		RoutePrefix: "/", // TODO: options for this?
		ExternalURL: externalUrl,
		// TODO: use these?
		/*
			ListenAddress        string
			ReadTimeout          time.Duration
			MaxConnections       int
			MetricsPath          string
			UseLocalAssets       bool
			UserAssetsPath       string
			ConsoleTemplatesPath string
			ConsoleLibrariesPath string
			EnableLifecycle      bool
			EnableAdminAPI       bool
		*/
		Version: &web.PrometheusVersion{
			Version:   version.Version,
			Revision:  version.Revision,
			Branch:    version.Branch,
			BuildUser: version.BuildUser,
			BuildDate: version.BuildDate,
			GoVersion: version.GoVersion,
		},
	}

	webHandler := web.New(logger, webOptions)
	reloadables = append(reloadables, proxyconfig.WrapPromReloadable(webHandler))
	webHandler.Ready()

	apiRouter := route.New()
	webHandler.Getv1API().Register(apiRouter.WithPrefix("/api/v1"))

	// Create our router
	r := httprouter.New()

	// TODO: configurable metrics path
	r.HandlerFunc("GET", "/metrics", prometheus.Handler().ServeHTTP)

	r.NotFound = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Have our fallback rules
		if strings.HasPrefix(r.URL.Path, "/api/") {
			apiRouter.ServeHTTP(w, r)
		} else if strings.HasPrefix(r.URL.Path, "/debug") {
			http.DefaultServeMux.ServeHTTP(w, r)
		} else {
			// all else we send direct to the local prometheus UI
			webHandler.GetRouter().ServeHTTP(w, r)
		}
	})

	if err := reloadConfig(reloadables...); err != nil {
		logrus.Fatalf("Error loading config: %s", err)
	}

	// Our own checks on top of the config
	checkConfig := func() error {
		// check for any recording rules, if we find any lets log a fatal and stop
		// https://github.com/jacksontj/promxy/issues/74
		for _, rule := range ruleManager.Rules() {
			if _, ok := rule.(*rules.RecordingRule); ok {
				return fmt.Errorf("Promxy doesn't support recording rules: %s", rule)
			}
		}
		return nil
	}

	if err := checkConfig(); err != nil {
		logrus.Fatalf("Error checking config: %v", err)
	}

	close(reloadReady)

	// Set up access logger
	var accessLogOut io.Writer
	switch strings.ToLower(opts.AccessLogDestination) {
	case "stderr":
		accessLogOut = os.Stderr
	case "stdout":
		accessLogOut = os.Stdout
	case "none":
	default:
		logrus.Fatalf("Invalid AccessLogDestination: %s", opts.AccessLogDestination)
	}

	var handler http.Handler
	if accessLogOut == nil {
		handler = r
	} else {
		handler = logging.NewApacheLoggingHandler(r, logging.LogToWriter(accessLogOut))
	}

	srv := &http.Server{
		Addr:    opts.BindAddr,
		Handler: handler,
	}

	go func() {
		logrus.Infof("promxy starting")
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("Error listening: %v", err)
		}
	}()

	// wait for signals etc.
	for {
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGHUP:
				log.Infof("Reloading config")
				if err := reloadConfig(reloadables...); err != nil {
					log.Errorf("Error reloading config: %s", err)
				}
				if err := checkConfig(); err != nil {
					logrus.Errorf("Error checking config: %v", err)
				}
			case syscall.SIGTERM, syscall.SIGINT:
				log.Infof("promxy exiting")
				cancel()
				srv.Shutdown(ctx)
				return
			default:
				log.Errorf("Uncaught signal: %v", sig)
			}

		}
	}
}

// sendAlerts implements the rules.NotifyFunc for a Notifier.
// It filters any non-firing alerts from the input.
func sendAlerts(n *notifier.Manager, externalURL string) rules.NotifyFunc {
	return func(ctx context.Context, expr string, alerts ...*rules.Alert) error {
		var res []*notifier.Alert

		for _, alert := range alerts {
			// Only send actually firing alerts.
			if alert.State == rules.StatePending {
				continue
			}
			a := &notifier.Alert{
				StartsAt:     alert.FiredAt,
				Labels:       alert.Labels,
				Annotations:  alert.Annotations,
				GeneratorURL: externalURL + strutil.TableLinkForExpression(expr),
			}
			if !alert.ResolvedAt.IsZero() {
				a.EndsAt = alert.ResolvedAt
			}
			res = append(res, a)
		}

		if len(alerts) > 0 {
			n.Send(res...)
		}
		return nil
	}
}

func startsOrEndsWithQuote(s string) bool {
	return strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "'") ||
		strings.HasSuffix(s, "\"") || strings.HasSuffix(s, "'")
}

// computeExternalURL computes a sanitized external URL from a raw input. It infers unset
// URL parts from the OS and the given listen address.
func computeExternalURL(u, listenAddr string) (*url.URL, error) {
	if u == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		_, port, err := net.SplitHostPort(listenAddr)
		if err != nil {
			return nil, err
		}
		u = fmt.Sprintf("http://%s:%s/", hostname, port)
	}

	if startsOrEndsWithQuote(u) {
		return nil, fmt.Errorf("URL must not begin or end with quotes")
	}

	eu, err := url.Parse(u)
	if err != nil {
		return nil, err
	}

	ppref := strings.TrimRight(eu.Path, "/")
	if ppref != "" && !strings.HasPrefix(ppref, "/") {
		ppref = "/" + ppref
	}
	eu.Path = ppref

	return eu, nil
}
