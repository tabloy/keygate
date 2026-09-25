package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/pkg/response"
)

// The middleware layer answers with the same envelope as every
// handler, by calling the same code rather than by copying its shape.
//
// It used to build the envelope itself, under a comment saying that
// avoided a circular import. There was no cycle to avoid: pkg/response
// imports log/slog, net/http and gin and nothing of ours. What the
// copy did produce was drift, which is what a second copy of a shape
// always produces in the end. One path here answered INTERNAL where
// the whole rest of the API answers INTERNAL_ERROR, and no test could
// see it, because a contract check that reads call sites cannot read a
// gin.H literal assembled by hand.
//
// These wrappers exist only to add the Abort. response.Err writes a
// body but does not stop the chain, and a middleware that writes
// without aborting lets the request carry on to the handler it just
// refused.

func abortWithError(c *gin.Context, status int, code, message string) {
	response.Err(c, status, code, message)
	c.Abort()
}

func abortWithErrorDetails(c *gin.Context, status int, code, message string, details any) {
	response.ErrWithDetails(c, status, code, message, details)
	c.Abort()
}

// abortInternal answers 500 and records the cause for the operator.
func abortInternal(c *gin.Context, cause error) {
	response.Internal(c, cause)
	c.Abort()
}
