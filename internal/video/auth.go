package video

// auth.go — Bearer-token introspection middleware. video-svc has no
// session store of its own; it asks dock to verify tokens via
// /internal/v1/auth/verify (cached 30s in the SDK). Mirrors
// packtunnel/wg/projects.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	ctxKeyUserID      = "user_id"
	ctxKeyUserRole    = "user_role"
	ctxKeyWorkspaceID = "workspace_id"
)

// requireAuthViaDock extracts Bearer → Dock.AuthVerify, then sets
// user_id / user_role / workspace_id on the gin context. Video has no
// admin-only routes — every authenticated user manages projects inside
// their own workspace.
func (p *Plugin) requireAuthViaDock() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractAccessToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		res, err := p.Dock.AuthVerify(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid session"})
			return
		}
		c.Set(ctxKeyUserID, res.UserID)
		c.Set(ctxKeyUserRole, res.Role)
		c.Set(ctxKeyWorkspaceID, res.WorkspaceID)
		c.Next()
	}
}

// extractAccessToken: Bearer header → ?access_token= → cookie. Same
// fallback chain as dock so iOS / browser clients work the same.
func extractAccessToken(c *gin.Context) string {
	if v := strings.TrimSpace(c.GetHeader("Authorization")); v != "" {
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			return strings.TrimSpace(v[7:])
		}
	}
	if v := strings.TrimSpace(c.Query("access_token")); v != "" {
		return v
	}
	if v, err := c.Cookie("access_token"); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}
