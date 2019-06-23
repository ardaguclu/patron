package patron

import (
	"os"
	"strconv"
	"sync"

	"github.com/beatlabs/patron/errors"
	"github.com/beatlabs/patron/info"
	"github.com/beatlabs/patron/log"
	"github.com/beatlabs/patron/log/zerolog"
	"github.com/beatlabs/patron/sync/http"
	"github.com/beatlabs/patron/trace"
	jaeger "github.com/uber/jaeger-client-go"
)

var logSetupOnce sync.Once

// Setup set's up metrics and default logging.
func Setup(name, version string) error {
	lvl, ok := os.LookupEnv("PATRON_LOG_LEVEL")
	if !ok {
		lvl = string(log.InfoLevel)
	}

	info.UpsertConfig("log_level", lvl)
	hostname, err := os.Hostname()
	if err != nil {
		return errors.Wrap(err, "failed to get hostname")
	}
	info.UpdateHost(hostname)

	f := map[string]interface{}{
		"srv":  name,
		"ver":  version,
		"host": hostname,
	}
	logSetupOnce.Do(func() {
		err = log.Setup(zerolog.Create(log.Level(lvl)), f)
	})

	return err
}

// Builder definition.
type Builder struct {
	errors        []error
	name          string
	version       string
	routes        []http.Route
	middlewares   []http.MiddlewareFunc
	healthCheck   http.HealthCheckFunc
	components    []Component
	sighupHandler func()
}

// New builder constructor.
func New(name string, version string) *Builder {
	b := &Builder{}
	var ers []error
	if name == "" {
		b.errors = append(ers, errors.New("name is required"))
		return b
	}
	if version == "" {
		version = "dev"
	}

	b.name = name
	b.version = version
	b.healthCheck = http.DefaultHealthCheck

	err := Setup(name, version)
	if err != nil {
		ers = append(ers, err)
	}

	info.UpdateName(name)
	info.UpdateVersion(version)

	err = setupDefaultTracing(name, version)
	if err != nil {
		ers = append(ers, err)
	}

	b.errors = ers
	return b
}

// WithRoutes adds routes to the service.
func (b *Builder) WithRoutes(rr ...http.Route) *Builder {
	if len(rr) == 0 {
		b.errors = append(b.errors, errors.New("routes are empty"))
	}
	b.routes = rr
	return b
}

// WithMiddlewares adds middlewares to the service.
func (b *Builder) WithMiddlewares(mm ...http.MiddlewareFunc) *Builder {
	if len(mm) == 0 {
		b.errors = append(b.errors, errors.New("middlewares are empty"))
	}
	b.middlewares = mm
	return b
}

// WithHealthCheck adds a custom health check to the service.
func (b *Builder) WithHealthCheck(hcf http.HealthCheckFunc) *Builder {
	if hcf == nil {
		b.errors = append(b.errors, errors.New("health check function is nil"))
	}
	b.healthCheck = hcf
	return b
}

// WithComponents adds custom components to the service.
func (b *Builder) WithComponents(cc ...Component) *Builder {
	if len(cc) == 0 {
		b.errors = append(b.errors, errors.New("components are empty"))
	}
	b.components = cc
	return b
}

// WithDocs adds docs support to the service.
func (b *Builder) WithDocs(file string) *Builder {
	if err := info.ImportDoc(file); err != nil {
		b.errors = append(b.errors, errors.New("failed to import doc file"))
	}
	return b
}

// WithSIGHUP adds custom SIGHUP handling to the service.
func (b *Builder) WithSIGHUP(handler func()) *Builder {
	if handler == nil {
		b.errors = append(b.errors, errors.New("sighub handler is nil"))
	}
	b.sighupHandler = handler
	return b
}

// Run the service.
func (b *Builder) Run() error {
	if len(b.errors) > 0 {
		return errors.Aggregate(b.errors...)
	}

	defer func() {
		err := trace.Close()
		if err != nil {
			log.Errorf("failed to close trace %v", err)
		}
	}()

	httpCmp, err := b.createHTTPComponent()
	if err != nil {
		return err
	}
	b.components = append(b.components, httpCmp)

	b.setupInfo()

	s, err := new(b.components, b.sighupHandler)
	if err != nil {
		return err
	}
	return s.Run()
	//TODO: fix cli to support the above
}

func setupDefaultTracing(name, version string) error {
	var err error

	host, ok := os.LookupEnv("PATRON_JAEGER_AGENT_HOST")
	if !ok {
		host = "0.0.0.0"
	}
	port, ok := os.LookupEnv("PATRON_JAEGER_AGENT_PORT")
	if !ok {
		port = "6831"
	}
	agent := host + ":" + port
	info.UpsertConfig("jaeger-agent", agent)
	tp, ok := os.LookupEnv("PATRON_JAEGER_SAMPLER_TYPE")
	if !ok {
		tp = jaeger.SamplerTypeProbabilistic
	}
	info.UpsertConfig("jaeger-agent-sampler-type", tp)
	var prmVal = 0.0
	var prm = "0.0"

	if prm, ok := os.LookupEnv("PATRON_JAEGER_SAMPLER_PARAM"); ok {
		prmVal, err = strconv.ParseFloat(prm, 64)
		if err != nil {
			return errors.Wrap(err, "env var for jaeger sampler param is not valid")
		}
	}

	info.UpsertConfig("jaeger-agent-sampler-param", prm)
	log.Infof("setting up default tracing %s, %s with param %s", agent, tp, prm)
	return trace.Setup(name, version, agent, tp, prmVal)
}

func (b *Builder) createHTTPComponent() (Component, error) {
	var err error
	var portVal = int64(50000)
	port, ok := os.LookupEnv("PATRON_HTTP_DEFAULT_PORT")
	if ok {
		portVal, err = strconv.ParseInt(port, 10, 64)
		if err != nil {
			return nil, errors.Wrap(err, "env var for HTTP default port is not valid")
		}
	}
	port = strconv.FormatInt(portVal, 10)
	log.Infof("creating default HTTP component at port %s", port)

	options := []http.OptionFunc{
		http.Port(int(portVal)),
	}

	if b.healthCheck != nil {
		options = append(options, http.HealthCheck(b.healthCheck))
	}

	if b.routes != nil {
		options = append(options, http.Routes(b.routes))
	}

	if b.middlewares != nil && len(b.middlewares) > 0 {
		options = append(options, http.Middlewares(b.middlewares...))
	}

	cp, err := http.New(options...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create default HTTP component")
	}

	return cp, nil
}

func (b *Builder) setupInfo() {
	for _, c := range b.components {
		info.AppendComponent(c.Info())
	}
}
