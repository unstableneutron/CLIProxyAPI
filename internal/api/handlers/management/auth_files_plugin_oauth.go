package management

import (
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// pluginAuthStartMetadata limits login metadata to provider options, excluding
// unrelated host query parameters such as management credentials.
func pluginAuthStartMetadata(c *gin.Context) map[string]any {
	if c == nil {
		return nil
	}
	values := make(url.Values)
	for _, key := range []string{"login_method", "start_url", "region", "idc_region", "idc_start_url", "scopes"} {
		if items := c.QueryArray(key); len(items) > 1 {
			values[key] = items
		} else if value := strings.TrimSpace(c.Query(key)); value != "" {
			values.Set(key, value)
		}
	}
	return queryValuesToMetadata(values)
}
