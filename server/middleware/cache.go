package middleware

import "github.com/gin-gonic/gin"

// NoStoreByDefault marks every response as not storable by any cache, unless
// the handler sets its own Cache-Control afterwards (system_3 #11494).
//
// Why a default and not a header per handler: a response with no
// Cache-Control leaves the decision to whatever sits between this server and
// the browser, and a proxy or CDN may decide by the URL alone, for example by
// a file extension at the end of the path. Nothing this API serves is meant
// for a shared cache, and a download must be made fresh each time it is asked
// for. So the safe value is the default and a handler opts out by name: the
// few that do (a profile image, a card asset) send "private".
//
// It runs before the handlers, so it also covers the 401 and 403 an auth
// middleware answers with and the router's own 404.
func NoStoreByDefault() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}
