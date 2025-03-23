package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
	"github.com/pgx-contrib/pgxotel"
	"github.com/pgx-contrib/pgxslog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelpropagation "go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace"
	"gopkg.in/natefinch/lumberjack.v2"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type Item struct {
	PK   int    `json:"pk"`
	Data string `json:"data"`
}

type service struct {
	db *pgxpool.Pool
}

type ctxLogKey struct{}

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests received",
		},
		[]string{"method", "endpoint"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Histogram of response time for handler in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "endpoint"},
	)
	dbQueryDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "db_query_duration_seconds",
			Help:    "Histogram of database query durations",
			Buckets: prometheus.DefBuckets,
		},
	)
	logger = slog.New(slog.NewJSONHandler(&lumberjack.Logger{
		Filename:   "/tmp/app/app.log",
		MaxSize:    1,
		MaxBackups: 5,
		MaxAge:     30,
		Compress:   false,
	}, nil))
	tracer     = otel.Tracer("app")
	propagator = otel.GetTextMapPropagator()
)

func init() {
	prometheus.MustRegister(httpRequestsTotal, httpRequestDuration, dbQueryDuration)
}

func loggerFromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxLogKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return logger
}

func contextWithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxLogKey{}, l)
}

func initTracer() func(context.Context) error {
	ctx := context.Background()

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
			semconv.ServiceName("app"),
		)),
	)
	otel.SetTracerProvider(tp)

	otel.SetTextMapPropagator(otelpropagation.TraceContext{})

	return tp.Shutdown
}

func handleJSONResponse(ctx context.Context, w http.ResponseWriter, data interface{}, err error, logMessage string) {
	log := loggerFromContext(ctx)
	switch {
	case err != nil:
		log.Error(logMessage, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case data == nil:
		log.Error(logMessage)
		w.WriteHeader(http.StatusNoContent)
	default:
		if err := json.NewEncoder(w).Encode(data); err != nil {
			log.Error("Error encoding JSON", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			uuidV7, err := uuid.NewV7()
			if err != nil {
				logger.Error("Failed to generate UUIDv7", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			requestID = uuidV7.String()
		}
		r.Header.Set("X-Request-ID", requestID)
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		ctx := r.Context()
		span := trace.SpanFromContext(ctx)

		traceID := ""
		if span != nil {
			span.SetAttributes(attribute.String("request.id", requestID))
			traceID = span.SpanContext().TraceID().String()
		}

		reqLogger := logger.With(
			slog.String("requestID", requestID),
			slog.String("traceID", traceID),
		)
		ctx = contextWithLogger(ctx, reqLogger)
		r = r.WithContext(ctx)

		reqLogger.Info("Incoming traceparent", "traceparent", r.Header.Get("traceparent"))

		propagator.Inject(ctx, otelpropagation.HeaderCarrier(w.Header()))

		nrw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(nrw, r)
		duration := time.Since(start)

		reqLogger.Info("Incoming request", "method", r.Method, "url", r.URL.Path)
		reqLogger.Info("Response sent", "status", nrw.statusCode, "duration", duration.String())
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (s *service) executeDBQuery(ctx context.Context, query string, args ...interface{}) (pgx.Rows, error) {
	ctx, span := tracer.Start(ctx, "executeDBQuery")
	defer span.End()
	timer := prometheus.NewTimer(dbQueryDuration)
	defer timer.ObserveDuration()

	loggerFromContext(ctx).Debug("Executing query", "query", query)
	return s.db.Query(ctx, query, args...)
}

func instrumentHandler(handlerFunc http.HandlerFunc, endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		timer := prometheus.NewTimer(httpRequestDuration.WithLabelValues(r.Method, endpoint))
		defer timer.ObserveDuration()

		httpRequestsTotal.WithLabelValues(r.Method, endpoint).Inc()
		handlerFunc(w, r)
	}
}

func (s *service) selfCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.db.Ping(ctx); err != nil {
		handleJSONResponse(ctx, w, nil, err, "Database self-check failed")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *service) addItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var item Item
	if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := s.executeDBQuery(ctx, "INSERT INTO my_table (data) VALUES ($1) RETURNING pk", item.Data)
	if err != nil {
		handleJSONResponse(ctx, w, nil, err, "Failed to insert item")
		return
	}
	defer rows.Close()
	rows.Next()
	if err := rows.Scan(&item.PK); err != nil {
		handleJSONResponse(ctx, w, nil, err, "Failed to scan item after insert")
		return
	}
	handleJSONResponse(ctx, w, item, nil, "Item inserted successfully")
}

func (s *service) getItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := mux.Vars(r)["id"]
	rows, err := s.executeDBQuery(ctx, "SELECT pk, data FROM my_table WHERE pk = $1", id)
	if err != nil {
		handleJSONResponse(ctx, w, nil, err, "Failed to fetch item")
		return
	}
	defer rows.Close()
	if !rows.Next() {
		handleJSONResponse(ctx, w, nil, nil, "Item not found")
		return
	}
	var item Item
	if err := rows.Scan(&item.PK, &item.Data); err != nil {
		handleJSONResponse(ctx, w, nil, err, "Failed to scan item")
		return
	}
	handleJSONResponse(ctx, w, item, nil, "Item retrieved successfully")
}

func (s *service) getAllItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.executeDBQuery(ctx, "SELECT pk, data FROM my_table")
	if err != nil {
		handleJSONResponse(ctx, w, nil, err, "Failed to fetch all items")
		return
	}
	items := []Item{}
	var item Item
	_, err = pgx.ForEachRow(rows, []any{&item.PK, &item.Data}, func() error {
		items = append(items, item)
		return nil
	})
	if err != nil {
		handleJSONResponse(ctx, w, nil, err, "Failed to scan item")
		return
	}
	handleJSONResponse(ctx, w, items, nil, "All items retrieved successfully")
}

func main() {
	shutdown := initTracer()
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			logger.Error("Tracer shutdown error", "error", err)
		}
	}()

	dbConfig, err := pgxpool.ParseConfig(os.Getenv("PG"))
	if err != nil {
		logger.Error("Failed to parse DB config", "error", err)
		os.Exit(1)
	}

	dbConfig.ConnConfig.Tracer = multitracer.New(
		&pgxotel.QueryTracer{Name: "github.com/gstarikov/2025-03-AUA-3-traces"},
		&tracelog.TraceLog{
			Logger:   &pgxslog.Logger{ContextKey: ctxLogKey{}},
			LogLevel: tracelog.LogLevelTrace,
		},
	)

	pgPool, err := pgxpool.NewWithConfig(context.Background(), dbConfig)
	if err != nil {
		logger.Error("Database connection failed", "error", err)
		os.Exit(1)
	}

	srvc := service{db: pgPool}
	r := mux.NewRouter()
	r.Use(requestIDMiddleware, loggingMiddleware)

	r.Handle("/metrics", promhttp.Handler())
	r.Handle("/self-check", instrumentHandler(srvc.selfCheck, "/self-check")).Methods(http.MethodGet)
	r.Handle("/items", instrumentHandler(srvc.addItem, "/items")).Methods(http.MethodPost)
	r.Handle("/items/{id}", instrumentHandler(srvc.getItem, "/items/{id}")).Methods(http.MethodGet)
	r.Handle("/items", instrumentHandler(srvc.getAllItems, "/items")).Methods(http.MethodGet)

	wrapperHandler := otelhttp.NewHandler(r, "http request",
		otelhttp.WithSpanNameFormatter(func(operation string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)

	s := &http.Server{
		Addr:           ":8080",
		Handler:        wrapperHandler,
		ReadTimeout:    1 * time.Second,
		WriteTimeout:   1 * time.Second,
		MaxHeaderBytes: 1 << 10,
	}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signalChan
		logger.Info("Shutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			logger.Error("Server shutdown failed", "error", err)
		}
		logger.Info("Server gracefully stopped")
	}()

	logger.Info("Server running on :8080")
	if err := s.ListenAndServe(); errors.Is(err, http.ErrServerClosed) {
		logger.Error("Server failed", "error", err)
	}
}
