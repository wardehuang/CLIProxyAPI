package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
)

// GetXAIClientVersion returns the version used by the core xAI chat executor.
func (h *Handler) GetXAIClientVersion(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"xai-client-version": executor.XAIClientVersion()})
}
