package main

import (
        "context"
        "database/sql"
        "fmt"
        "html/template"
        "log"
        "net/http"
        "os"
        "runtime"
        "strconv"
        "time"

        "github.com/grafana/pyroscope-go"
        _ "github.com/lib/pq"
        "github.com/prometheus/client_golang/prometheus"
        "github.com/prometheus/client_golang/prometheus/promhttp"
        "github.com/uptrace/opentelemetry-go-extra/otelsql"
        "go.opentelemetry.io/otel"
        "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
        "go.opentelemetry.io/otel/sdk/resource"
        sdktrace "go.opentelemetry.io/otel/sdk/trace"
        semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
        "go.opentelemetry.io/otel/trace"
)

var (
        db     *sql.DB
        tracer trace.Tracer

        // Prometheus metrics - akan discrape oleh Alloy
        httpRequestsTotal = prometheus.NewCounterVec(
                prometheus.CounterOpts{
                        Name: "http_requests_total",
                        Help: "Total number of HTTP requests",
                },
                []string{"method", "endpoint", "status"},
        )

        httpRequestDuration = prometheus.NewHistogramVec(
                prometheus.HistogramOpts{
                        Name: "http_request_duration_seconds",
                        Help: "HTTP request duration in seconds",
                },
                []string{"method", "endpoint"},
        )

        ordersCreatedTotal = prometheus.NewCounter(
                prometheus.CounterOpts{
                        Name: "orders_created_total",
                        Help: "Total number of orders created",
                },
        )
)

type Product struct {
        ID    int
        Name  string
        Price float64
}

type Order struct {
        ID        int
        ProductID int
        Quantity  int
        Total     float64
        CreatedAt time.Time
}

func init() {
        // Register Prometheus metrics
        prometheus.MustRegister(httpRequestsTotal)
        prometheus.MustRegister(httpRequestDuration)
        prometheus.MustRegister(ordersCreatedTotal)
}

func main() {
        // Initialize Pyroscope profiling - kirim ke Alloy yang forward ke Pyroscope
        _, err := pyroscope.Start(pyroscope.Config{
                ApplicationName: getEnv("OTEL_SERVICE_NAME", "backend"),
                ServerAddress:   getEnv("PYROSCOPE_ENDPOINT", "http://pyroscope-distributor.monitoring.svc.cluster.local:4040"),
                Logger:          pyroscope.StandardLogger,
                ProfileTypes: []pyroscope.ProfileType{
                        pyroscope.ProfileCPU,
                        pyroscope.ProfileAllocObjects,
                        pyroscope.ProfileAllocSpace,
                },
        })
        if err != nil {
                log.Printf("Failed to start Pyroscope: %v", err)
        }

        // Initialize OpenTelemetry tracing - kirim ke Alloy via OTLP
        if err := initTracing(); err != nil {
                log.Fatalf("Failed to initialize tracing: %v", err)
        }

        // Initialize database dengan tracing
        if err := initDB(); err != nil {
                log.Fatalf("Failed to initialize database: %v", err)
        }
        defer db.Close()

        // Create tables dan insert sample data
        if err := setupDatabase(); err != nil {
                log.Fatalf("Failed to setup database: %v", err)
        }

        // Setup HTTP routes dengan middleware
        http.HandleFunc("/", withMiddleware(homeHandler))
        http.HandleFunc("/checkout", withMiddleware(checkoutHandler))
        http.HandleFunc("/success", withMiddleware(successHandler))
        http.Handle("/metrics", promhttp.Handler()) // Endpoint untuk Alloy scrape metrics

        log.Printf("🚀 E-commerce app starting on :8080")
        log.Fatal(http.ListenAndServe(":8080", nil))
}

// initTracing setup OpenTelemetry untuk kirim trace ke Alloy
func initTracing() error {
        // Create OTLP HTTP exporter ke Alloy
        exporter, err := otlptracehttp.New(
                context.Background(),
                otlptracehttp.WithEndpoint(getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://alloy.monitoring.svc.cluster.local:4318")),
                otlptracehttp.WithInsecure(),
        )
        if err != nil {
                return fmt.Errorf("failed to create OTLP exporter: %w", err)
        }

        // Create resource dengan service name
        res, err := resource.New(
                context.Background(),
                resource.WithAttributes(
                        semconv.ServiceName(getEnv("OTEL_SERVICE_NAME", "backend")),
                        semconv.ServiceVersion("1.0.0"),
                ),
        )
        if err != nil {
                return fmt.Errorf("failed to create resource: %w", err)
        }

        // Create trace provider dengan always sampling
        tp := sdktrace.NewTracerProvider(
                sdktrace.WithBatcher(exporter),
                sdktrace.WithResource(res),
                sdktrace.WithSampler(sdktrace.AlwaysSample()),
        )

        otel.SetTracerProvider(tp)
        tracer = otel.Tracer("ecommerce-app")

        log.Printf("✅ Tracing initialized, sending to: %s", getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://alloy.monitoring.svc.cluster.local:4318"))
        return nil
}

// initDB setup database dengan otelsql untuk tracing
func initDB() error {
        dsn := getEnv("DATABASE_DSN", "postgres://user:password@postgres:5432/dbname?sslmode=disable")

        // Gunakan otelsql untuk database tracing - span akan muncul di Tempo
        var err error
        db, err = otelsql.Open("postgres", dsn)
        if err != nil {
                return fmt.Errorf("failed to open database: %w", err)
        }

        if err := db.Ping(); err != nil {
                return fmt.Errorf("failed to ping database: %w", err)
        }

        log.Printf("✅ Connected to PostgreSQL")
        return nil
}

func setupDatabase() error {
        ctx, span := tracer.Start(context.Background(), "setup_database")
        defer span.End()

        // Create tables
        queries := []string{
                `CREATE TABLE IF NOT EXISTS products (
                        id SERIAL PRIMARY KEY,
                        name VARCHAR(255) NOT NULL,
                        price DECIMAL(10,2) NOT NULL
                )`,
                `CREATE TABLE IF NOT EXISTS orders (
                        id SERIAL PRIMARY KEY,
                        product_id INTEGER REFERENCES products(id),
                        quantity INTEGER NOT NULL,
                        total DECIMAL(10,2) NOT NULL,
                        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
                )`,
        }

        for _, query := range queries {
                if _, err := db.ExecContext(ctx, query); err != nil {
                        return fmt.Errorf("failed to execute query: %w", err)
                }
        }

        // Insert sample products jika belum ada
        var count int
        err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM products").Scan(&count)
        if err != nil {
                return err
        }

        if count == 0 {
                products := []Product{
                        {Name: "Laptop Gaming ROG", Price: 25000000},
                        {Name: "Mouse Gaming Logitech", Price: 450000},
                        {Name: "Keyboard Mechanical RGB", Price: 1200000},
                        {Name: "Monitor 27 inch 4K", Price: 4500000},
                        {Name: "Headset Gaming", Price: 800000},
                }

                for _, product := range products {
                        _, err := db.ExecContext(ctx, 
                                "INSERT INTO products (name, price) VALUES ($1, $2)",
                                product.Name, product.Price)
                        if err != nil {
                                return err
                        }
                }
                log.Printf("✅ Sample products inserted")
        }

        return nil
}

// withMiddleware menambahkan tracing, metrics, dan logging
func withMiddleware(handler http.HandlerFunc) http.HandlerFunc {
        return func(w http.ResponseWriter, r *http.Request) {
                start := time.Now()

                // Create span untuk request - akan muncul di Tempo
                ctx, span := tracer.Start(r.Context(), fmt.Sprintf("%s %s", r.Method, r.URL.Path))
                defer span.End()

                r = r.WithContext(ctx)

                // Wrap response writer untuk capture status code
                wrapped := &responseWriter{ResponseWriter: w, statusCode: 200}

                // Call handler
                handler(wrapped, r)

                // Record metrics - akan discrape oleh Alloy
                duration := time.Since(start).Seconds()
                httpRequestsTotal.WithLabelValues(r.Method, r.URL.Path, strconv.Itoa(wrapped.statusCode)).Inc()
                httpRequestDuration.WithLabelValues(r.Method, r.URL.Path).Observe(duration)

                // Log ke stdout - akan dikumpulkan oleh Alloy untuk Loki
                log.Printf("📊 %s %s %d %v", r.Method, r.URL.Path, wrapped.statusCode, time.Since(start))
        }
}

type responseWriter struct {
        http.ResponseWriter
        statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
        rw.statusCode = code
        rw.ResponseWriter.WriteHeader(code)
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
        ctx, span := tracer.Start(r.Context(), "home_handler")
        defer span.End()

        // Get products dari database - span akan muncul di Tempo
        products, err := getProducts(ctx)
        if err != nil {
                log.Printf("❌ Failed to get products: %v", err)
                http.Error(w, "Failed to get products", http.StatusInternalServerError)
                return
        }

        // Load template
        tmpl, err := template.ParseFiles("templates/index.html")
        if err != nil {
                log.Printf("❌ Failed to parse template: %v", err)
                http.Error(w, "Template error", http.StatusInternalServerError)
                return
        }

        if err := tmpl.Execute(w, products); err != nil {
                log.Printf("❌ Failed to execute template: %v", err)
        }

        log.Printf("🏠 Home page served with %d products", len(products))
}

func checkoutHandler(w http.ResponseWriter, r *http.Request) {
        ctx, span := tracer.Start(r.Context(), "checkout_handler")
        defer span.End()

        if r.Method != "POST" {
                http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
                return
        }

        // Simulate CPU work untuk profiling - akan terlihat di Pyroscope
        simulateCPUWork()

        productID, _ := strconv.Atoi(r.FormValue("product_id"))
        quantity, _ := strconv.Atoi(r.FormValue("quantity"))

        if productID == 0 || quantity == 0 {
                http.Error(w, "Invalid product or quantity", http.StatusBadRequest)
                return
        }

        // Get product - database span
        product, err := getProduct(ctx, productID)
        if err != nil {
                log.Printf("❌ Product not found: %v", err)
                http.Error(w, "Product not found", http.StatusNotFound)
                return
        }

        // Create order - database span
        total := product.Price * float64(quantity)
        orderID, err := createOrder(ctx, productID, quantity, total)
        if err != nil {
                log.Printf("❌ Failed to create order: %v", err)
                http.Error(w, "Failed to create order", http.StatusInternalServerError)
                return
        }

        // Increment metric
        ordersCreatedTotal.Inc()

        log.Printf("🛒 Order created: ID=%d, Product=%s, Qty=%d, Total=Rp%.0f", 
                orderID, product.Name, quantity, total)

        http.Redirect(w, r, fmt.Sprintf("/success?order_id=%d", orderID), http.StatusSeeOther)
}

func successHandler(w http.ResponseWriter, r *http.Request) {
        ctx, span := tracer.Start(r.Context(), "success_handler")
        defer span.End()

        orderID, _ := strconv.Atoi(r.URL.Query().Get("order_id"))

        // Get order dan product - database spans
        order, err := getOrder(ctx, orderID)
        if err != nil {
                log.Printf("❌ Order not found: %v", err)
                http.Error(w, "Order not found", http.StatusNotFound)
                return
        }

        product, err := getProduct(ctx, order.ProductID)
        if err != nil {
                log.Printf("❌ Product not found: %v", err)
                http.Error(w, "Product not found", http.StatusNotFound)
                return
        }

        // Load template
        tmpl, err := template.ParseFiles("templates/success.html")
        if err != nil {
                log.Printf("❌ Failed to parse template: %v", err)
                http.Error(w, "Template error", http.StatusInternalServerError)
                return
        }

        data := struct {
                Order   Order
                Product Product
        }{
                Order:   *order,
                Product: *product,
        }

        if err := tmpl.Execute(w, data); err != nil {
                log.Printf("❌ Failed to execute template: %v", err)
        }

        log.Printf("✅ Success page served for order %d", orderID)
}

// Database functions dengan tracing
func getProducts(ctx context.Context) ([]Product, error) {
        ctx, span := tracer.Start(ctx, "db_get_products")
        defer span.End()

        rows, err := db.QueryContext(ctx, "SELECT id, name, price FROM products ORDER BY id")
        if err != nil {
                return nil, err
        }
        defer rows.Close()

        var products []Product
        for rows.Next() {
                var p Product
                if err := rows.Scan(&p.ID, &p.Name, &p.Price); err != nil {
                        return nil, err
                }
                products = append(products, p)
        }

        return products, nil
}

func getProduct(ctx context.Context, id int) (*Product, error) {
        ctx, span := tracer.Start(ctx, "db_get_product")
        defer span.End()

        var p Product
        err := db.QueryRowContext(ctx, "SELECT id, name, price FROM products WHERE id = $1", id).
                Scan(&p.ID, &p.Name, &p.Price)
        if err != nil {
                return nil, err
        }

        return &p, nil
}

func createOrder(ctx context.Context, productID, quantity int, total float64) (int, error) {
        ctx, span := tracer.Start(ctx, "db_create_order")
        defer span.End()

        var orderID int
        err := db.QueryRowContext(ctx,
                "INSERT INTO orders (product_id, quantity, total) VALUES ($1, $2, $3) RETURNING id",
                productID, quantity, total).Scan(&orderID)

        return orderID, err
}

func getOrder(ctx context.Context, id int) (*Order, error) {
        ctx, span := tracer.Start(ctx, "db_get_order")
        defer span.End()

        var o Order
        err := db.QueryRowContext(ctx,
                "SELECT id, product_id, quantity, total, created_at FROM orders WHERE id = $1", id).
                Scan(&o.ID, &o.ProductID, &o.Quantity, &o.Total, &o.CreatedAt)
        if err != nil {
                return nil, err
        }

        return &o, nil
}

// simulateCPUWork untuk profiling visibility di Pyroscope
func simulateCPUWork() {
        // CPU intensive work
        for i := 0; i < 2000000; i++ {
                _ = i * i * i
        }

        // Memory allocation untuk profiling
        data := make([]int, 10000)
        for i := range data {
                data[i] = i
        }

        runtime.GC()
}

func getEnv(key, defaultValue string) string {
        if value := os.Getenv(key); value != "" {
                return value
        }
        return defaultValue
}
