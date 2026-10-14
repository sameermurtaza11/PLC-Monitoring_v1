package web

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// pipelineStatus shows the health of the sample pipeline: the bus queues
// with every sample dropped so far, the storage module (buffer, errors) and
// the scan statistics of each PLC. Administrators only.
func (s *Server) pipelineStatus(c *gin.Context) {
	if s.pipeline == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "pipeline statistics are not available"})
		return
	}
	c.JSON(http.StatusOK, s.pipeline())
}
