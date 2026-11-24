package handlers

import (
	"errors"
	"log"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
)

// RecoverPanics turns a panic in a later handler into a 500 in the usual
// error envelope, rather than a dropped connection. It is mounted first, so
// it wraps every route (system_3 #12255).
//
// A handler that opened a transaction ends it on a panic too: every
// db.Begin() is followed by `defer tx.RollbackUnlessCommitted()`, and Go runs
// deferred calls while a panic unwinds, before this recover sees it.
//
// It logs the method, the route template (no ids, no query), the panic value
// and the stack. It never logs the request's headers or body: gin.Recovery()
// prints the headers in debug mode, and masks Authorization but not the
// auth_token cookie.
func RecoverPanics() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// net/http's sentinel for "abort this response": let the
			// server handle it as it means to.
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			route := c.FullPath()
			if route == "" {
				route = "(no route)"
			}
			log.Printf("[Recovery] panic in %s %s: %v\n%s", c.Request.Method, route, rec, debug.Stack())
			if !c.Writer.Written() {
				RespondWithError(c, http.StatusInternalServerError, "Internal server error")
			}
			c.Abort()
		}()
		c.Next()
	}
}
