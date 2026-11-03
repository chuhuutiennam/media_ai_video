package controlplane

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// ginLogger logs each HTTP request via zap.
// Level: DEBUG for 2xx/3xx, WARN for 4xx, ERROR for 5xx.
func ginLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		if q := c.Request.URL.RawQuery; q != "" {
			path += "?" + q
		}

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		fields := make([]zap.Field, 0, 8)
		fields = append(fields,
			zap.String("method",     c.Request.Method),
			zap.String("path",       path),
			zap.Int("status",        status),
			zap.Int64("latency_ms",  latency.Milliseconds()),
			zap.String("ip",         c.ClientIP()),
			zap.Int("bytes",         c.Writer.Size()),
		)
		if id := c.GetHeader("X-Request-ID"); id != "" {
			fields = append(fields, zap.String("request_id", id))
		}
		if err := c.Errors.ByType(gin.ErrorTypePrivate).String(); err != "" {
			fields = append(fields, zap.String("error", err))
		}

		switch {
		case status >= 500:
			zap.L().Error("http request", fields...)
		case status >= 400:
			zap.L().Warn("http request", fields...)
		default:
			zap.L().Debug("http request", fields...)
		}
	}
}
