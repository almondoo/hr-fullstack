// HR SaaS backend - 開発用エントリポイント（最小スケルトン）
// Gin + GORM(PostgreSQL, pgx/v5 driver) + 標準ライブラリ slog。
// /healthz: 死活、 /readyz: DB接続込みのレディネス。
//
// マルチテナント分離は PostgreSQL RLS + tenant_id 方式（決定事項）。
// 実運用ではリクエストごとにトランザクション内で `SET LOCAL app.tenant_id = ...`
// を実行し、RLSポリシーを効かせること（docs/04_tech_stack.md の GORM×RLS ノート参照）。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	port := getenv("APP_PORT", "8080")

	db, err := connectDB(os.Getenv("DATABASE_URL"), logger)
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	sqlDB, err := db.DB()
	if err != nil {
		logger.Error("failed to get sql.DB", "error", err)
		os.Exit(1)
	}
	defer sqlDB.Close()

	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger(logger))
	r.Use(securityHeaders())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := sqlDB.PingContext(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second, // Slowloris 対策
	}

	go func() {
		logger.Info("server starting", "port", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// グレースフルシャットダウン
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down server")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("forced shutdown", "error", err)
	}
	logger.Info("server stopped")
}

// connectDB は GORM(PostgreSQL) 接続を確立し、PostgreSQL の起動待ちのためにリトライする。
func connectDB(dsn string, logger *slog.Logger) (*gorm.DB, error) {
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}

	var db *gorm.DB
	var err error
	for attempt := 1; attempt <= 10; attempt++ {
		db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger:                 gormlogger.Default.LogMode(gormlogger.Warn),
			SkipDefaultTransaction: true, // 性能。トランザクションは明示的に張る（RLS の SET LOCAL 用にも必要）
		})
		if err == nil {
			sqlDB, derr := db.DB()
			if derr == nil {
				sqlDB.SetMaxOpenConns(10)
				sqlDB.SetMaxIdleConns(5)
				sqlDB.SetConnMaxLifetime(time.Hour)
				pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				err = sqlDB.PingContext(pingCtx)
				cancel()
				if err == nil {
					logger.Info("database connected")
					return db, nil
				}
			} else {
				err = derr
			}
		}
		logger.Warn("waiting for database", "attempt", attempt, "error", err)
		time.Sleep(2 * time.Second)
	}
	return nil, err
}

func requestLogger(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		// 個人情報や秘密はログに出さない（メソッド/パス/ステータスのみ）
		logger.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	}
}

// securityHeaders は最低限のセキュリティヘッダを付与する（本番は gin-contrib/secure + CSP を推奨）。
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Cross-Origin-Opener-Policy", "same-origin")
		c.Next()
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
