package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gocraft/health"
	"github.com/justinas/alice"
	"github.com/paulbellamy/ratecounter"
)

const mwStatusRespond = 666

var GlobalRate = ratecounter.NewRateCounter(1 * time.Second)

type TykMiddleware interface {
	Init()
	Base() *BaseMiddleware
	Config() (interface{}, error)
	ProcessRequest(w http.ResponseWriter, r *http.Request, conf interface{}) (error, int) // Handles request
	IsEnabledForSpec() bool
	Name() string
}

func createDynamicMiddleware(name string, isPre, useSession bool, baseMid *BaseMiddleware) func(http.Handler) http.Handler {
	dMiddleware := &DynamicMiddleware{
		BaseMiddleware:      baseMid,
		MiddlewareClassName: name,
		Pre:                 isPre,
		UseSession:          useSession,
	}

	return createMiddleware(dMiddleware)
}

// Generic middleware caller to make extension easier
func createMiddleware(mw TykMiddleware) func(http.Handler) http.Handler {
	// construct a new instance
	mw.Init()

	// Pull the configuration
	mwConf, err := mw.Config()
	if err != nil {
		log.Fatal("[Middleware] Configuration load failed")
	}

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			job := instrument.NewJob("MiddlewareCall")
			meta := health.Kvs{
				"from_ip":  requestIP(r),
				"method":   r.Method,
				"endpoint": r.URL.Path,
				"raw_url":  r.URL.String(),
				"size":     strconv.Itoa(int(r.ContentLength)),
				"mw_name":  mw.Name(),
			}
			eventName := mw.Name() + "." + "executed"
			job.EventKv("executed", meta)
			job.EventKv(eventName, meta)
			startTime := time.Now()

			if mw.Base().Spec.CORS.OptionsPassthrough && r.Method == "OPTIONS" {
				h.ServeHTTP(w, r)
				return
			}
			err, errCode := mw.ProcessRequest(w, r, mwConf)
			if err != nil {
				handler := ErrorHandler{mw.Base()}
				handler.HandleError(w, r, err.Error(), errCode)
				meta["error"] = err.Error()
				job.TimingKv("exec_time", time.Since(startTime).Nanoseconds(), meta)
				job.TimingKv(eventName+".exec_time", time.Since(startTime).Nanoseconds(), meta)
				return
			}

			// Special code, bypasses all other execution
			if errCode != mwStatusRespond {
				// No error, carry on...
				meta["bypass"] = "1"
				h.ServeHTTP(w, r)
			}

			job.TimingKv("exec_time", time.Since(startTime).Nanoseconds(), meta)
			job.TimingKv(eventName+".exec_time", time.Since(startTime).Nanoseconds(), meta)
		})
	}
}

func appendMiddleware(chain *[]alice.Constructor, mw TykMiddleware) {
	if mw.IsEnabledForSpec() {
		*chain = append(*chain, createMiddleware(mw))
	}
}

type TykResponseHandler interface {
	Init(interface{}, *APISpec) error
	HandleResponse(http.ResponseWriter, *http.Response, *http.Request, *SessionState) error
}

func responseProcessorByName(name string) TykResponseHandler {
	switch name {
	case "header_injector":
		return &HeaderInjector{}
	case "response_body_transform":
		return &ResponseTransformMiddleware{}
	case "header_transform":
		return &HeaderTransform{}
	}
	return nil
}

func handleResponseChain(chain []TykResponseHandler, rw http.ResponseWriter, res *http.Response, req *http.Request, ses *SessionState) error {
	for _, rh := range chain {
		if err := rh.HandleResponse(rw, res, req, ses); err != nil {
			return err
		}
	}
	return nil
}
