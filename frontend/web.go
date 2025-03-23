package main

import (
	"context"
	otelpropagation "go.opentelemetry.io/otel/propagation"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/natefinch/lumberjack"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
	"log/slog"
)

type Item struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

const backendURL = "http://app:8080"

var (
	requestCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Count of HTTP requests",
		},
		[]string{"path", "method"},
	)
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duration of HTTP requests",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"path", "method"},
	)
	logger = initLogger()
	client = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
)

func main() {
	initLogger()
	initMetrics()
	shutdown := initTracer()
	defer shutdown(context.Background())

	http.Handle("/metrics", promhttp.Handler())
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("/web/static"))))
	http.Handle("/", otelhttp.NewHandler(withMiddleware(handleIndex), "/"))
	http.Handle("/api/items", otelhttp.NewHandler(withMiddleware(handleItems), "/api/items"))

	logger.Info("Backend service listening", "port", 8081)
	log.Fatal(http.ListenAndServe(":8081", nil))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	tmpl := template.Must(template.ParseFiles("/web/index.html"))
	tmpl.Execute(w, nil)
}

func handleItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := getLogger(ctx)

	backendReq, err := http.NewRequestWithContext(ctx, r.Method, backendURL+"/items", r.Body)
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	backendReq.Header = r.Header

	resp, err := client.Do(backendReq)
	if err != nil {
		http.Error(w, "Backend service unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)

	log.Info("Proxied request", "method", r.Method, "status", resp.StatusCode)
}

func withMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = uuid.New().String()
			r.Header.Set("X-Request-ID", rid)
		}
		ctx := context.WithValue(r.Context(), "requestID", rid)

		// Enrich logger with request ID and trace ID
		ctx = context.WithValue(ctx, "logger", logger.With(
			slog.String("requestID", rid),
			slog.String("traceID", getTraceID(ctx)),
		))
		r = r.WithContext(ctx)

		requestCounter.WithLabelValues(r.URL.Path, r.Method).Inc()
		timer := prometheus.NewTimer(requestDuration.WithLabelValues(r.URL.Path, r.Method))
		defer timer.ObserveDuration()

		next(w, r)

		logger.Info("Request completed",
			"requestID", rid,
			"traceID", getTraceID(ctx),
			"method", r.Method,
			"path", r.URL.Path,
			"duration", time.Since(start).String(),
		)
	}
}

func getLogger(ctx context.Context) *slog.Logger {
	logVal := ctx.Value("logger")
	if logVal == nil {
		return logger.With("requestID", "unknown")
	}
	return logVal.(*slog.Logger)
}

// getTraceID extracts the trace ID from the context
func getTraceID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		return "unknown"
	}
	return span.SpanContext().TraceID().String()
}

func initLogger() *slog.Logger {
	logFile := &lumberjack.Logger{
		Filename:   "/tmp/web/app.log",
		MaxSize:    1,
		MaxBackups: 5,
		MaxAge:     30,
		Compress:   false,
	}
	return slog.New(slog.NewJSONHandler(logFile, nil))
}

func initMetrics() {
	prometheus.MustRegister(requestCounter)
	prometheus.MustRegister(requestDuration)
}

func initTracer() func(context.Context) error {
	ctx := context.Background()
	otel.SetTextMapPropagator(otelpropagation.TraceContext{})

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint(os.Getenv("OTEL")),
	)
	if err != nil {
		logger.Error("Failed to create OTLP exporter", "error", err)
		os.Exit(1)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("web"),
		)),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown
}
