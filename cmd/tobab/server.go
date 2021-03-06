package main

import (
	"context"
	"html/template"
	"net"
	"net/http"
	"net/http/httputil"
	"net/rpc"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/gnur/tobab"
	"github.com/gnur/tobab/muxlogger"
	"github.com/gnur/tobab/storm"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/argon2"
)

var version = "manual build"

type Tobab struct {
	fqdn       string
	key        []byte
	config     tobab.Config
	logger     *logrus.Entry
	maxAge     time.Duration
	defaultAge time.Duration
	templates  *template.Template
	confLoc    string
	db         tobab.Database
	server     *http.Server
}

func run(confLoc string) {
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		ForceColors:   true,
		FullTimestamp: true,
	})

	cfg, err := tobab.LoadConf(confLoc)
	if err != nil {
		logger.WithError(err).Fatal("Failed loading config")
	}

	if lvl, err := logrus.ParseLevel(cfg.Loglevel); err == nil {
		logger.SetLevel(lvl)
	}

	certmagic.DefaultACME.Agreed = true
	certmagic.DefaultACME.Email = cfg.Email

	if cfg.Staging {
		certmagic.DefaultACME.CA = certmagic.LetsEncryptStagingCA
	}

	//only use provided salt if it makes any sense at all
	//otherwise use the default salt, shouldn't be a problem
	salt := []byte(cfg.Salt)
	secret := []byte(cfg.Secret)

	//set secret that goth uses
	os.Setenv("SESSION_SECRET", string(secret))

	//transform provided salt and secret into a 32 byte key that can be used by paseto
	key := argon2.IDKey(secret, salt, 4, 4*1024, 2, 32)

	if version == "" {
		version = "unknown"
	}

	db, err := storm.New(cfg.DatabasePath)
	if err != nil {
		logger.WithError(err).WithField("location", cfg.DatabasePath).Fatal("Unable to initialize database")
	}
	defer db.Close()

	app := Tobab{
		key:     key,
		config:  cfg,
		logger:  logger.WithField("version", version),
		maxAge:  720 * time.Hour,
		fqdn:    "https://" + cfg.Hostname,
		confLoc: confLoc,
		db:      db,
	}

	if age, err := time.ParseDuration(cfg.DefaultTokenAge); err != nil {
		app.defaultAge = 720 * time.Hour
	} else {
		app.defaultAge = age
	}

	if age, err := time.ParseDuration(cfg.MaxTokenAge); err != nil {
		app.maxAge = 24 * 365 * time.Hour
	} else {
		app.maxAge = age
	}

	app.templates, err = loadTemplates()
	if err != nil {
		logger.WithError(err).Fatal("unable to load templates")
	}
	go app.startServer()
	go app.startRPCServer()

	c := make(chan os.Signal, 1)
	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	// SIGKILL, SIGQUIT or SIGTERM (Ctrl+/) will not be caught.
	signal.Notify(c, os.Interrupt)

	// Block until we receive our signal.
	<-c
	app.logger.Info("shutting down")
}

func (app *Tobab) startRPCServer() {
	err := rpc.Register(app)
	if err != nil {
		app.logger.WithError(err).Error("Failed to register rpc")
		return
	}
	rpc.HandleHTTP()
	l, err := net.Listen("tcp", ":1234")
	if err != nil {
		app.logger.WithError(err).Error("Failed to start rpc listener")
		return
	}
	err = http.Serve(l, nil)
	if err != nil {
		app.logger.WithError(err).Error("Failed to start rpc http")
		return
	}

}

func (app *Tobab) restartServer() {

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	app.logger.Info("shutting down server")
	err := app.server.Shutdown(ctx)
	app.logger.WithError(err).Info("server was shut down")

	go app.startServer()
}

func (app *Tobab) startServer() {
	app.logger.Info("starting server")
	r := mux.NewRouter()
	certHosts := []string{app.config.Hostname}
	var err error

	app.logger.Debug("loading hosts")
	hosts, err := app.db.GetHosts()
	if err != nil {
		app.logger.WithError(err).Fatal("unable to load hosts")
	}

	for _, conf := range hosts {
		if conf.Type != "http" {
			app.logger.WithField("type", conf.Type).Fatal("Unsupported type, currently only http is supported")
		}

		proxy, err := generateProxy(conf.Hostname, conf.Backend)
		if err != nil {
			app.logger.WithError(err).WithField("host", conf.Hostname).Error("Failed creating proxy")
			continue
		}

		app.logger.WithField("host", conf.Hostname).Debug("starting proxy listener")
		r.Host(conf.Hostname).PathPrefix("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
		})
		certHosts = append(certHosts, conf.Hostname)
	}

	tobabRoutes := r.Host(app.config.Hostname).Subrouter()
	app.setTobabRoutes(tobabRoutes)

	r.Use(muxlogger.NewLogger(app.logger).Middleware)
	r.Use(handlers.CompressHandler)
	r.Use(app.getRBACMiddleware())

	magicListener, err := certmagic.Listen(certHosts)
	if err != nil {
		app.logger.WithError(err).Fatal("Failed getting certmagic listener")
	}

	srv := &http.Server{
		WriteTimeout: time.Second * 15,
		ReadTimeout:  time.Second * 15,
		IdleTimeout:  time.Second * 60,
		Handler:      r,
	}
	go func() {
		err = srv.Serve(magicListener)
		if err != nil {
			if err != http.ErrServerClosed {
				app.logger.WithError(err).Fatal("Failed starting magic listener")
			}
		}
	}()
	app.server = srv
}

func generateProxy(host, backend string) (http.Handler, error) {
	url, err := url.Parse(backend)
	if err != nil {
		return nil, err
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.Header.Add("X-Forwarded-Host", url.Hostname())
			req.Header.Add("X-Origin-Host", host)
			req.Host = url.Host
			req.URL.Host = url.Host
			req.URL.Scheme = url.Scheme

		}, Transport: &http.Transport{
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConns:        100,
			Dial: (&net.Dialer{
				Timeout:   600 * time.Second,
				KeepAlive: 300 * time.Second,
			}).Dial,
		}}

	return proxy, nil
}
