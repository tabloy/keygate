package middleware

import "github.com/gin-gonic/gin"

// abortWithError sends a standardized error envelope and aborts the request.
// This mirrors the format from pkg/response without importing it (avoiding circular deps).
func abortWithError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"success": false,
		"error":   gin.H{"code": code, "message": message},
	})
}
