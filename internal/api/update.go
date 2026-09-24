package api

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// UpdateStatus reports the installed version against what the project has
// published, with the changelog of everything in between. `?refresh=true` skips
// the cached check — the console's "检查更新" button.
func (h *Handlers) UpdateStatus(c *echo.Context) error {
	st, err := h.Deps.Updater.Status(c.Request().Context(), c.QueryParam("refresh") == "true")
	if err != nil {
		// The check failed outside this host: GitHub was unreachable, or its
		// rate limit was hit. Neither is a fault of the request.
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	return c.JSON(http.StatusOK, st)
}

// RunUpdate installs the newest release. It answers with a job: the download is
// tens of megabytes, and the service restarts itself moments after the job
// finishes, so the console polls the job rather than holding a request open
// across its own server's restart.
func (h *Handlers) RunUpdate(c *echo.Context) error {
	job, err := h.Deps.Jobs.Submit(c.Request().Context(), "self-update", h.Deps.Updater.Install)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, job)
}
