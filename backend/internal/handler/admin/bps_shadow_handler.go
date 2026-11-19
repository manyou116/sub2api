package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type bpsShadowAdmin interface {
	CreateBPSShadow(context.Context, int64, service.BPSShadowOptions) (*service.Account, error)
	BPSAuthorizationStatus(context.Context, int64, int64, string) (service.BPSAuthorization, error)
	StartBPSAuthorization(context.Context, int64, int64, string, []byte) (service.BPSAuthorization, error)
	CancelBPSAuthorization(context.Context, int64, int64, string) error
}

func (h *OpenAIOAuthHandler) CreateBPSShadow(c *gin.Context) {
	svc, ok := h.adminService.(bpsShadowAdmin)
	if !ok {
		c.Status(http.StatusNotImplemented)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	var options service.BPSShadowOptions
	if err != nil || id <= 0 || c.ShouldBindJSON(&options) != nil {
		response.BadRequest(c, "Invalid BPS shadow configuration")
		return
	}
	account, err := svc.CreateBPSShadow(c.Request.Context(), id, options)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.AccountFromService(account))
}

func (h *OpenAIOAuthHandler) BPSAuthorization(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	svc, ok := h.adminService.(bpsShadowAdmin)
	if !ok {
		c.Status(http.StatusNotImplemented)
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		c.Status(http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid BPS shadow ID")
		return
	}
	ctx := c.Request.Context()
	// Use a request header rather than a query parameter for the operation ID.
	session := c.GetHeader("X-BPS-Authorization")
	var view service.BPSAuthorization
	switch c.Request.Method {
	case http.MethodPost:
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 34<<10)
		var in struct {
			Method string          `json:"method"`
			Grant  json.RawMessage `json:"grant"`
		}
		decoder := json.NewDecoder(c.Request.Body)
		var trailing any
		if decoder.Decode(&in) != nil || decoder.Decode(&trailing) != io.EOF {
			response.BadRequest(c, "Invalid BPS authorization JSON")
			return
		}
		view, err = svc.StartBPSAuthorization(ctx, id, subject.UserID, in.Method, in.Grant)
	case http.MethodGet:
		view, err = svc.BPSAuthorizationStatus(ctx, id, subject.UserID, session)
	case http.MethodDelete:
		err = svc.CancelBPSAuthorization(ctx, id, subject.UserID, session)
	default:
		c.Status(http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, view)
}
