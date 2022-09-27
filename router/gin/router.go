// SPDX-License-Identifier: Apache-2.0

/*
	Package gin provides some basic implementations for building routers based on gin-gonic/gin
*/
package gin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/luraproject/lura/v2/config"
	"github.com/luraproject/lura/v2/core"
	"github.com/luraproject/lura/v2/logging"
	"github.com/luraproject/lura/v2/proxy"
	"github.com/luraproject/lura/v2/router"
	"github.com/luraproject/lura/v2/transport/http/server"
)

const logPrefix = "[SERVICE: Gin]"

var mu = &sync.Mutex{}

// RunServerFunc is a func that will run the http Server with the given params.
type RunServerFunc func(context.Context, config.ServiceConfig, http.Handler) error

// Config is the struct that collects the parts the router should be builded from
type Config struct {
	Engine         *gin.Engine
	Middlewares    []gin.HandlerFunc
	HandlerFactory HandlerFactory
	ProxyFactory   proxy.Factory
	Logger         logging.Logger
	RunServer      RunServerFunc
}

// DefaultFactory returns a gin router factory with the injected proxy factory and logger.
// It also uses a default gin router and the default HandlerFactory
func DefaultFactory(proxyFactory proxy.Factory, logger logging.Logger) router.Factory {
	return NewFactory(
		Config{
			Engine:         gin.Default(),
			Middlewares:    []gin.HandlerFunc{},
			HandlerFactory: EndpointHandler,
			ProxyFactory:   proxyFactory,
			Logger:         logger,
			RunServer:      server.RunServer,
		},
	)
}

// NewFactory returns a gin router factory with the injected configuration
func NewFactory(cfg Config) router.Factory {
	return factory{cfg}
}

type factory struct {
	cfg Config
}

// New implements the factory interface
func (rf factory) New() router.Router {
	return rf.NewWithContext(context.Background())
}

// NewWithContext implements the factory interface
func (rf factory) NewWithContext(ctx context.Context) router.Router {
	return ginRouter{
		cfg:        rf.cfg,
		ctx:        ctx,
		runServerF: rf.cfg.RunServer,
		mu:         new(sync.Mutex),
		urlCatalog: urlCatalog{
			mu:      new(sync.Mutex),
			catalog: map[string][]string{},
		},
	}
}

type ginRouter struct {
	cfg        Config
	ctx        context.Context
	runServerF RunServerFunc
	mu         *sync.Mutex
	urlCatalog urlCatalog
}

type urlCatalog struct {
	mu      *sync.Mutex
	catalog map[string][]string
}

// Run completes the router initialization and executes it
func (r ginRouter) Run(cfg config.ServiceConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()

	server.InitHTTPDefaultTransport(cfg)

	r.registerEndpointsAndMiddlewares(cfg)
	go r.tcpConn(cfg)
	// TODO: remove this ugly hack once https://github.com/gin-gonic/gin/pull/2692 and
	// https://github.com/gin-gonic/gin/issues/2862 are completely fixed
	go r.cfg.Engine.Run("XXXX")

	r.cfg.Logger.Info("[SERVICE: Gin] Listening on port:", cfg.Port)
	if err := r.runServerF(r.ctx, cfg, r.cfg.Engine); err != nil && err != http.ErrServerClosed {
		r.cfg.Logger.Error(logPrefix, err.Error())
	}

	r.cfg.Logger.Info(logPrefix, "Router execution ended")
}

func (r ginRouter) registerEndpointsAndMiddlewares(cfg config.ServiceConfig) {
	if cfg.Debug {
		r.cfg.Engine.Any("/__debug/*param", DebugHandler(r.cfg.Logger))
	}
	r.cfg.Engine.POST("/UpdateHost", UpdateHost(cfg))
	//r.cfg.Engine.POST("/modifyHost", modifyHost(cfg))
	endpointGroup := r.cfg.Engine.Group("/")
	endpointGroup.Use(r.cfg.Middlewares...)

	r.registerKrakendEndpoints(endpointGroup, cfg)

	if opts, ok := cfg.ExtraConfig[Namespace].(map[string]interface{}); ok {
		if v, ok := opts["auto_options"].(bool); ok && v {
			r.cfg.Logger.Debug(logPrefix, "Enabling the auto options endpoints")
			r.registerOptionEndpoints(endpointGroup)
		}
	}

}

func (r ginRouter) registerKrakendEndpoints(rg *gin.RouterGroup, cfg config.ServiceConfig) {
	// build and register the pipes and endpoints sequentially
	for _, c := range cfg.Endpoints {
		proxyStack, err := r.cfg.ProxyFactory.New(c)
		if err != nil {
			r.cfg.Logger.Error(logPrefix, "Calling the ProxyFactory", err.Error())
			continue
		}
		r.registerKrakendEndpoint(rg, c.Method, c, r.cfg.HandlerFactory(c, proxyStack), len(c.Backend))
	}
}

func (r ginRouter) registerKrakendEndpoint(rg *gin.RouterGroup, method string, e *config.EndpointConfig, h gin.HandlerFunc, total int) {
	method = strings.ToTitle(method)
	path := e.Endpoint
	if method != http.MethodGet && total > 1 {
		if !router.IsValidSequentialEndpoint(e) {
			r.cfg.Logger.Error(logPrefix, method, "endpoints with sequential proxy enabled only allow a non-GET in the last backend! Ignoring", path)
			return
		}
	}

	switch method {
	case http.MethodGet:
		rg.GET(path, h)
	case http.MethodPost:
		rg.POST(path, h)
	case http.MethodPut:
		rg.PUT(path, h)
	case http.MethodPatch:
		rg.PATCH(path, h)
	case http.MethodDelete:
		rg.DELETE(path, h)
	default:
		r.cfg.Logger.Error(logPrefix, "Unsupported method", method)
		return
	}

	r.urlCatalog.mu.Lock()
	defer r.urlCatalog.mu.Unlock()

	methods, ok := r.urlCatalog.catalog[path]
	if !ok {
		r.urlCatalog.catalog[path] = []string{method}
		return
	}
	r.urlCatalog.catalog[path] = append(methods, method)
}

func (r ginRouter) registerOptionEndpoints(rg *gin.RouterGroup) {
	r.urlCatalog.mu.Lock()
	defer r.urlCatalog.mu.Unlock()

	for path, methods := range r.urlCatalog.catalog {
		sort.Strings(methods)
		allowed := strings.Join(methods, ", ")

		rg.OPTIONS(path, func(c *gin.Context) {
			c.Header("Allow", allowed)
			c.Header(core.KrakendHeaderName, core.KrakendHeaderValue)
		})
	}

}

func (r *ginRouter) tcpConn(cfg config.ServiceConfig) {
	l, err := net.Listen("tcp", ":7001")
	if nil != err {
		r.cfg.Logger.Error(logPrefix, err.Error())
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
		if nil != err {
			r.cfg.Logger.Error(logPrefix, err.Error())
			continue
		}
		defer conn.Close()
		go r.ConnHandler(conn, cfg)
	}

}

func (r *ginRouter) ConnHandler(conn net.Conn, cfg config.ServiceConfig) {
	defer conn.Close()
	data := map[string]string{}

	json.NewDecoder(conn).Decode(data)
	cleaner := config.NewURIParser()
	mu.Lock()
	for _, j := range cfg.Endpoints {
		for k, i := range j.Backend {
			if k == 0 {
				for num, host := range i.Host {
					if host == cleaner.CleanHost(data["zombieIp"]) {
						i.Host = append(i.Host[:num], i.Host[num+1:]...)
					}
				}
				i.Host = append(i.Host, cleaner.CleanHost(data["newIp"]))
				fmt.Println(i.Host)
			}
		}
	}
	mu.Unlock()
}

/*
func modifyHost(cfg config.ServiceConfig data []byte) {

	data := map[string]string{}
	cleaner := config.NewURIParser()
	mu.Lock()
	for _, j := range cfg.Endpoints {
		for k, i := range j.Backend {
			if k == 0 {
			for num, host := range i.Host {
				if host ==  cleaner.CleanHost(data["zombieIp"]) {																i.Host = append(i.Host[:num], i.Host[num+1:]...)																				}
						}
					  i.Host = append(i.Host, cleaner.CleanHost(data["newIp"]))
					  fmt.Println(i.Host)																					}																				}																				}
		mu.Unlock()

}
*/

func UpdateHost(cfg config.ServiceConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		CurrentAddressTable := map[string]string{}
		cleaner := config.NewURIParser()
		json.NewDecoder(c.Request.Body).Decode(&CurrentAddressTable)
		c.Request.Body.Close()
		mu.Lock()
		for _, j := range cfg.Endpoints {
			for k, i := range j.Backend {
				if k == 0 {
					data := len(i.Host)
					copy1 := make([]string, 0)
					for z := 0; z < data; z++ {
						num := 0
						i.Host = append(i.Host[:num], i.Host[num+1:]...)
					}
					for _, n := range CurrentAddressTable {
						i.Host = append(i.Host, cleaner.CleanHost(n))
						copy1 = append(copy1, cleaner.CleanHost(n))
					}
					if len(copy1) <= 4 {
						for zz := 0; zz < 2; zz++ {
							i.Host = append(i.Host, copy1[:]...)
						}
					}
				}
			}

		}
		mu.Unlock()
	}

}
