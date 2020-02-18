package server

import (
	"fmt"
	"net/http"
	pprof "net/http/pprof"
	"regexp"

	"github.com/equinor/seismic-cloud/api/controller"

	jwt "github.com/dgrijalva/jwt-go"
	_ "github.com/equinor/seismic-cloud/api/docs" // docs is generated by Swag CLI, you have to import it.
	l "github.com/equinor/seismic-cloud/api/logger"
	claimsmiddleware "github.com/equinor/seismic-cloud/api/middleware/claims"
	"github.com/equinor/seismic-cloud/api/service"
	"github.com/equinor/seismic-cloud/api/service/store"
	jwtmiddleware "github.com/iris-contrib/middleware/jwt"
	prometheusmiddleware "github.com/iris-contrib/middleware/prometheus"
	"github.com/iris-contrib/swagger/v12"
	"github.com/iris-contrib/swagger/v12/swaggerFiles"
	"github.com/kataras/iris/v12"
	irisCtx "github.com/kataras/iris/v12/context"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type HTTPServer struct {
	service     APIService
	app         *iris.Application
	version     string
	hostAddr    string
	domains     string
	domainmail  string
	privKeyFile string
	certFile    string
	profile     bool
}

type APIService struct {
	manifestStore store.ManifestStore
	surfaceStore  store.SurfaceStore
	stitcher      service.Stitcher
}

type HTTPServerOption interface {
	apply(*HTTPServer) error
}

func DefaultHTTPServer() *HTTPServer {

	app := iris.Default()
	app.Logger().SetPrefix("iris: ")
	l.AddGoLogSource(app.Logger().SetOutput)
	return &HTTPServer{
		app:      app,
		hostAddr: "localhost:8080"}
}

func NewHTTPServer(opts ...HTTPServerOption) (hs *HTTPServer, err error) {
	hs = DefaultHTTPServer()
	for _, opt := range opts {
		err = opt.apply(hs)
		if err != nil {
			return nil, fmt.Errorf("Applying config failed: %v", err)
		}
	}
	hs.app.Use(iris.Gzip)

	if hs.service.manifestStore == nil {
		return nil, fmt.Errorf("Server cannot start, no manifest store set")
	}

	if hs.service.stitcher == nil {
		return nil, fmt.Errorf("Server cannot start, stitch command is empty")
	}

	if hs.service.surfaceStore == nil {
		return nil, fmt.Errorf("Server cannot start, no surface store set")
	}

	return hs, nil
}

func WithOAuth2(oauthOpt OAuth2Option) HTTPServerOption {

	return newFuncOption(func(hs *HTTPServer) error {
		sigKeySet, err := service.GetOIDCKeySet(oauthOpt.AuthServer)
		if err != nil {
			return fmt.Errorf("Couldn't get keyset: %v", err)
		}

		rsaJWTHandler := jwtmiddleware.New(jwtmiddleware.Config{
			ValidationKeyGetter: func(t *jwt.Token) (interface{}, error) {

				if t.Method.Alg() != "RS256" {
					return nil, fmt.Errorf("unexpected jwt signing method=%v", t.Header["alg"])
				}
				return sigKeySet[t.Header["kid"].(string)], nil

			},
			ContextKey:    "user-jwt",
			SigningMethod: jwt.SigningMethodRS256,
		})

		onRS256Pass := func(ctx irisCtx.Context, err error) {

			if err == nil || err.Error() == "unexpected jwt signing method=RS256" {
				return
			}
			jwtmiddleware.OnError(ctx, err)
		}
		hmacJWTHandler := jwtmiddleware.New(jwtmiddleware.Config{
			ValidationKeyGetter: func(t *jwt.Token) (interface{}, error) {

				if t.Method.Alg() != "HS256" {
					return nil, fmt.Errorf("unexpected jwt signing method=%v", t.Header["alg"])
				}
				return oauthOpt.ApiSecret, nil
			},
			ContextKey:    "service-jwt",
			SigningMethod: jwt.SigningMethodHS256,
			ErrorHandler:  onRS256Pass,
		})

		if len(oauthOpt.Issuer) == 0 {
			oauthOpt.Issuer = oauthOpt.AuthServer.String()
		}

		claimsHandler := claimsmiddleware.New(oauthOpt.Audience, oauthOpt.Issuer)

		auth := func(ctx irisCtx.Context) {
			hmacJWTHandler.Serve(ctx)
			serviceToken := ctx.Values().Get("service-jwt")
			if serviceToken == nil {
				rsaJWTHandler.Serve(ctx)
			}

		}
		hs.app.Use(auth)
		hs.app.Use(claimsHandler.Validate)
		return nil
	})
}

func (hs *HTTPServer) registerMacros() {
	manifestIDExpr := "^[a-zA-Z0-9\\-\\_]{1,100}$"
	manifestIDRegex, err := regexp.Compile(manifestIDExpr)
	if err != nil {
		panic(err)
	}

	hs.app.Macros().Get("string").RegisterFunc("idString", manifestIDRegex.MatchString)
}

func (hs *HTTPServer) registerEndpoints() {

	sc := controller.NewSurfaceController(hs.service.surfaceStore)

	hs.app.Post("/surface/{surfaceID:string idString() else 502}", sc.Upload)
	hs.app.Get("/surface/{surfaceID:string idString() else 502}", sc.Download)
	hs.app.Get("/surface", sc.List)
	hs.app.Get("/", func(ctx iris.Context) {
		_, err := ctx.HTML(hs.version)
		if err != nil {
			ctx.StatusCode(500)
		}
	})

	mc := controller.NewManifestController(hs.service.manifestStore)

	hs.app.Get("/manifest/{manifestID:string idString() else 502}", mc.Download)
	hs.app.Post("/manifest/{manifestID:string idString() else 502}", mc.Upload)

	hs.app.Get("/stitch/{manifestID:string idString() else 502}/{surfaceID: string idString() else 502}",
		controller.StitchSurfaceController(
			hs.service.manifestStore,
			hs.service.stitcher))
	hs.app.Get("/stitch/{manifestID:string idString() else 502}/dim/{dim:uint32}/{lineno:uint64}",
		controller.StitchDimController(
			hs.service.manifestStore,
			hs.service.stitcher))

}

func (hs *HTTPServer) Serve() error {
	hs.registerMacros()
	hs.registerEndpoints()

	config := &swagger.Config{
		URL: fmt.Sprintf("http://%s/swagger/doc.json", hs.hostAddr), //The url pointing to API definition
	}
	// use swagger middleware to
	hs.app.Get("/swagger/{any:path}", swagger.CustomWrapHandler(config, swaggerFiles.Handler))

	if hs.profile {
		// Activate Prometheus middleware if profiling is on
		metrics := iris.Default()

		l.AddGoLogSource(metrics.Logger().SetOutput)
		metrics.Get("/metrics", iris.FromStd(promhttp.Handler()))
		metrics.Get("/debug/pprof", iris.FromStd(pprof.Index))
		metrics.Get("/debug/pprof/cmdline", iris.FromStd(pprof.Cmdline))
		metrics.Get("/debug/pprof/profile", iris.FromStd(pprof.Profile))
		metrics.Get("/debug/pprof/symbol", iris.FromStd(pprof.Symbol))

		metrics.Get("/debug/pprof/goroutine", iris.FromStd(pprof.Handler("goroutine")))
		metrics.Get("/debug/pprof/heap", iris.FromStd(pprof.Handler("heap")))
		metrics.Get("/debug/pprof/threadcreate", iris.FromStd(pprof.Handler("threadcreate")))
		metrics.Get("/debug/pprof/block", iris.FromStd(pprof.Handler("block")))

		err := metrics.Build()
		if err != nil {
			panic(err)
		}
		metricsServer := &http.Server{Addr: ":8081", Handler: metrics}

		go func() {
			err := metricsServer.ListenAndServe()
			if err != nil {
				l.LogE("Server shutdown", err)
			}
		}()
	}

	return hs.app.Run(iris.Addr(hs.hostAddr))
}

func WithManifestStore(manifestStore store.ManifestStore) HTTPServerOption {

	return newFuncOption(func(hs *HTTPServer) (err error) {
		hs.service.manifestStore = manifestStore
		return
	})
}

func WithSurfaceStore(surfaceStore store.SurfaceStore) HTTPServerOption {

	return newFuncOption(func(hs *HTTPServer) (err error) {
		hs.service.surfaceStore = surfaceStore
		return
	})
}

func WithHostAddr(hostAddr string) HTTPServerOption {

	return newFuncOption(func(hs *HTTPServer) (err error) {
		hs.hostAddr = hostAddr
		return
	})
}

func WithAPIVersion(version string) HTTPServerOption {

	return newFuncOption(func(hs *HTTPServer) (err error) {
		hs.version = version
		return
	})
}

func WithProfiling() HTTPServerOption {

	return newFuncOption(func(hs *HTTPServer) (err error) {
		hs.profile = true

		m := prometheusmiddleware.New("Metrics", 0.3, 1.2, 5.0)
		hs.app.Use(m.ServeHTTP)
		hs.app.OnAnyErrorCode(func(ctx iris.Context) {
			// error code handlers are not sharing the same middleware as other routes, so we have
			// to call them inside their body.
			m.ServeHTTP(ctx)

		})
		return
	})
}

func WithStitcher(stitcher service.Stitcher) HTTPServerOption {

	return newFuncOption(func(hs *HTTPServer) (err error) {

		hs.service.stitcher = stitcher
		return
	})
}
