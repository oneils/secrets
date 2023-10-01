// Package server provides rest-like api and serves static assets as well
package server

import (
	"context"
	"html/template"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/didip/tollbooth/v7"
	"github.com/didip/tollbooth_chi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/render"
	"github.com/pkg/errors"

	log "github.com/go-pkgz/lgr"
	um "github.com/go-pkgz/rest"

	"github.com/umputun/secrets/backend/app/messager"
	"github.com/umputun/secrets/backend/app/store"
)

// Server is a rest with store
type Server struct {
	Messager       Messager
	PinSize        int
	MaxPinAttempts int
	MaxExpire      time.Duration
	WebRoot        string
	Version        string
	Domain         string
	TemplateCache  map[string]*template.Template
}

// Messager interface making and loading messages
type Messager interface {
	MakeMessage(duration time.Duration, msg, pin string) (result *store.Message, err error)
	LoadMessage(key, pin string) (msg *store.Message, err error)
}

// Run the lister and request's router, activate rest server
func (s Server) Run(ctx context.Context) error {
	log.Printf("[INFO] activate rest server")

	httpServer := &http.Server{
		Addr:              ":8080",
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		if httpServer != nil {
			if clsErr := httpServer.Close(); clsErr != nil {
				log.Printf("[ERROR] failed to close proxy http server, %v", clsErr)
			}
		}
	}()

	err := httpServer.ListenAndServe()
	log.Printf("[WARN] http server terminated, %s", err)

	if !errors.Is(err, http.ErrServerClosed) {
		return errors.Wrap(err, "server failed")
	}
	return nil
}

func (s Server) routes() chi.Router {
	router := chi.NewRouter()

	router.Use(middleware.RequestID, middleware.RealIP, um.Recoverer(log.Default()))
	router.Use(middleware.Throttle(1000), middleware.Timeout(60*time.Second))
	router.Use(um.AppInfo("secrets", "Umputun", s.Version), um.Ping, um.SizeLimit(64*1024))
	router.Use(tollbooth_chi.LimitHandler(tollbooth.NewLimiter(10, nil)))

	router.Route("/api/v1", func(r chi.Router) {
		r.Use(Logger(log.Default()))
		r.Post("/message", s.saveMessageCtrl)
		r.Get("/message/{key}/{pin}", s.getMessageCtrl)
		r.Get("/params", s.getParamsCtrl)
	})

	router.Get("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		render.PlainText(w, r, "User-agent: *\nDisallow: /api/\nDisallow: /show/\n")
	})

	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1") {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, JSON{"error": "not found"})
			return
		}

		s.render(w, http.StatusNotFound, "404.tmpl.html", baseTmpl, "not found")
	})

	router.Get("/", s.indexCtrl)
	router.Post("/generate-link", s.generateLink)
	router.Get("/message/{key}", s.showMessageView)
	router.Post("/load-message", s.loadMessage)
	router.Get("/about", s.aboutView)

	s.fileServer(router, "/", truncatedFileSystem{http.Dir(s.WebRoot)})

	return router
}

// POST /v1/message
func (s Server) saveMessageCtrl(w http.ResponseWriter, r *http.Request) {
	request := struct {
		Message string
		Exp     int
		Pin     string
	}{}

	if err := render.DecodeJSON(r.Body, &request); err != nil {
		log.Printf("[WARN] can't bind request %v", request)
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, JSON{"error": err.Error()})
		return
	}

	if len(request.Pin) != s.PinSize {
		log.Printf("[WARN] incorrect pin size %d", len(request.Pin))
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, JSON{"error": "Incorrect pin size"})
		return
	}

	msg, err := s.Messager.MakeMessage(time.Second*time.Duration(request.Exp), request.Message, request.Pin)
	if err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, JSON{"error": err.Error()})
		return
	}
	render.Status(r, http.StatusCreated)
	render.JSON(w, r, JSON{"key": msg.Key, "exp": msg.Exp})
}

// GET /v1/message/{key}/{pin}
func (s Server) getMessageCtrl(w http.ResponseWriter, r *http.Request) {

	key, pin := chi.URLParam(r, "key"), chi.URLParam(r, "pin")
	if key == "" || pin == "" || len(pin) != s.PinSize {
		log.Print("[WARN] no valid key or pin in get request")
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, JSON{"error": "no key or pin passed"})
		return
	}

	serveRequest := func() (status int, res JSON) {
		msg, err := s.Messager.LoadMessage(key, pin)
		if err != nil {
			log.Printf("[WARN] failed to load key %v", key)
			if err == messager.ErrBadPinAttempt {
				return http.StatusExpectationFailed, JSON{"error": err.Error()}
			}
			return http.StatusBadRequest, JSON{"error": err.Error()}
		}
		return http.StatusOK, JSON{"key": msg.Key, "message": string(msg.Data)}
	}

	// make sure serveRequest works constant time on any branch
	st := time.Now()
	status, res := serveRequest()
	time.Sleep(time.Millisecond*100 - time.Since(st))
	render.Status(r, status)
	render.JSON(w, r, res)
}

// GET /params
func (s Server) getParamsCtrl(w http.ResponseWriter, r *http.Request) {
	params := struct {
		PinSize        int `json:"pin_size"`
		MaxPinAttempts int `json:"max_pin_attempts"`
		MaxExpSecs     int `json:"max_exp_sec"`
	}{
		PinSize:        s.PinSize,
		MaxPinAttempts: s.MaxPinAttempts,
		MaxExpSecs:     int(s.MaxExpire.Seconds()),
	}
	render.JSON(w, r, params)
}

// serves static files
func (s Server) fileServer(r chi.Router, path string, root http.FileSystem) {
	log.Printf("[INFO] run file server for %s", root)
	fs := http.StripPrefix(path, http.FileServer(root))

	path += "*"

	r.Handle(path, http.StripPrefix("/static", fs))
}

// truncatedFileSystem is a wrapper for http.FileSystem  to disable directory listings.
// It serves index.html for directories if present and return 404 for others
type truncatedFileSystem struct {
	fs http.FileSystem
}

// Open returns file or index.html for directories
func (nfs truncatedFileSystem) Open(path string) (http.File, error) {
	f, err := nfs.fs.Open(path)
	if err != nil {
		return nil, err
	}

	s, err := f.Stat()
	if err != nil {
		closeErr := f.Close()
		if closeErr != nil {
			return nil, closeErr
		}

		return nil, err
	}
	if s.IsDir() {
		index := filepath.Join(path, "index.html")
		if _, err := nfs.fs.Open(index); err != nil {
			closeErr := f.Close()
			if closeErr != nil {
				return nil, closeErr
			}

			return nil, err
		}
	}

	return f, nil
}
