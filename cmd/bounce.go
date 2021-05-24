package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo"
	"github.com/lib/pq"
)

type bouncesWrap struct {
	Results []models.Bounce `json:"results"`

	Total   int `json:"total"`
	PerPage int `json:"per_page"`
	Page    int `json:"page"`
}

type sendgridNotif struct {
	Email     string `json:"email"`
	Timestamp int64  `json:"timestamp"`
	Event     string `json:"event"`
}

type sesTimestamp time.Time

type sesNotif struct {
	NotifType string `json:"notificationType"`
	Bounce    struct {
		BounceType string `json:"bounceType"`
	} `json:"bounce"`
	Mail struct {
		Timestamp        sesTimestamp        `json:"timestamp"`
		HeadersTruncated bool                `json:"headersTruncated"`
		Destination      []string            `json:"destination"`
		Headers          []map[string]string `json:"headers"`
	} `json:"mail"`
}

// handleGetBounces handles retrieval of bounce records.
func handleGetBounces(c echo.Context) error {
	var (
		app = c.Get("app").(*App)
		pg  = getPagination(c.QueryParams(), 20, 50)
		out bouncesWrap

		id, _     = strconv.Atoi(c.Param("id"))
		campID, _ = strconv.Atoi(c.QueryParam("campaign_id"))
		source    = c.FormValue("source")
		orderBy   = c.FormValue("order_by")
		order     = c.FormValue("order")
	)

	// Fetch one list.
	single := false
	if id > 0 {
		single = true
	}

	// Sort params.
	if !strSliceContains(orderBy, bounceQuerySortFields) {
		orderBy = "created_at"
	}
	if order != sortAsc && order != sortDesc {
		order = sortDesc
	}

	stmt := fmt.Sprintf(app.queries.QueryBounces, orderBy, order)
	if err := db.Select(&out.Results, stmt, id, campID, 0, source, pg.Offset, pg.Limit); err != nil {
		app.log.Printf("error fetching bounces: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			app.i18n.Ts("globals.messages.errorFetching",
				"name", "{globals.terms.bounce}", "error", pqErrMsg(err)))
	}
	if len(out.Results) == 0 {
		out.Results = []models.Bounce{}
		return c.JSON(http.StatusOK, okResp{out})
	}

	if single {
		return c.JSON(http.StatusOK, okResp{out.Results[0]})
	}

	// Meta.
	out.Total = out.Results[0].Total
	out.Page = pg.Page
	out.PerPage = pg.PerPage

	return c.JSON(http.StatusOK, okResp{out})
}

// handleDeleteBounces handles bounce deletion, either a single one (ID in the URI), or a list.
func handleDeleteBounces(c echo.Context) error {
	var (
		app    = c.Get("app").(*App)
		pID    = c.Param("id")
		all, _ = strconv.ParseBool(c.QueryParam("all"))
		IDs    = pq.Int64Array{}
	)

	// Is it an /:id call?
	if pID != "" {
		id, _ := strconv.ParseInt(pID, 10, 64)
		if id < 1 {
			return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidID"))
		}
		IDs = append(IDs, id)
	} else if !all {
		// Multiple IDs.
		i, err := parseStringIDs(c.Request().URL.Query()["id"])
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest,
				app.i18n.Ts("globals.messages.invalidID", "error", err.Error()))
		}

		if len(i) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest,
				app.i18n.Ts("globals.messages.invalidID"))
		}
		IDs = i
	}

	if _, err := app.queries.DeleteBounces.Exec(IDs); err != nil {
		app.log.Printf("error deleting bounces: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			app.i18n.Ts("globals.messages.errorDeleting",
				"name", "{globals.terms.bounce}", "error", pqErrMsg(err)))
	}

	return c.JSON(http.StatusOK, okResp{true})
}

// handleBounceWebhook renders the HTML preview of a template.
func handleBounceWebhook(c echo.Context) error {
	var (
		app     = c.Get("app").(*App)
		service = c.Param("service")

		bounces []models.Bounce
	)

	switch service {
	// Native postback.
	case "":
		var b models.Bounce
		if err := c.Bind(&b); err != nil {
			return err
		}

		if err := validateBounceFields(b, app); err != nil {
			return err
		}

		b.Email = strings.ToLower(b.Email)

		if len(b.Meta) == 0 {
			b.Meta = json.RawMessage("{}")
		}

		if b.CreatedAt.Year() == 0 {
			b.CreatedAt = time.Now()
		}

		bounces = append(bounces, b)

	// Amazon SES.
	case "ses":
		b, err := parseSESNotif(c, app)
		if err != nil {
			return err
		}
		bounces = append(bounces, b)

	// SendGrid.
	case "sendgrid":
		bs, err := parseSendgridNotif(c, app)
		if err != nil {
			return err
		}
		bounces = append(bounces, bs...)

	default:
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.Ts("bounces.unknownService"))
	}

	// Record bounces.
	for _, b := range bounces {
		if err := app.bounce.Record(b); err != nil {
			app.log.Printf("error recording bounce: %v", err)
		}
	}

	return c.JSON(http.StatusOK, okResp{true})
}

func validateBounceFields(b models.Bounce, app *App) error {
	if b.Email == "" && b.SubscriberUUID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidData"))
	}

	if b.Email != "" && !subimporter.IsEmail(b.Email) {
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidEmail"))
	}

	if b.SubscriberUUID != "" && !reUUID.MatchString(b.SubscriberUUID) {
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidUUID"))
	}

	return nil
}

func parseSESNotif(c echo.Context, app *App) (models.Bounce, error) {
	var (
		event sesNotif
		b     models.Bounce
	)

	// Read the request body instead of using using c.Bind() to read to save the entire raw request as meta.
	rawReq, err := ioutil.ReadAll(c.Request().Body)
	if err != nil {
		app.log.Printf("error reading ses notification body: %v", err)
		return b, echo.NewHTTPError(http.StatusBadRequest, app.i18n.Ts("globals.messages.internalError"))
	}

	if err := json.Unmarshal(rawReq, &event); err != nil {
		app.log.Printf("error parsing ses notification: %v", err)
		return b, echo.NewHTTPError(http.StatusBadRequest, app.i18n.Ts("globals.messages.invalidData"))
	}

	if len(event.Mail.Destination) == 0 {
		return b, echo.NewHTTPError(http.StatusBadRequest, app.i18n.Ts("globals.messages.invalidData"))
	}

	typ := "soft"
	if event.Bounce.BounceType == "Permanent" {
		typ = "hard"
	}

	// Look for the campaign ID in headers.
	campUUID := ""
	if !event.Mail.HeadersTruncated {
		for _, h := range event.Mail.Headers {
			key, ok := h["name"]
			if !ok || key != models.EmailHeaderCampaignUUID {
				continue
			}

			campUUID, ok = h["value"]
			if !ok {
				continue
			}
			break
		}
	}

	return models.Bounce{
		Email:        event.Mail.Destination[0],
		CampaignUUID: campUUID,
		Type:         typ,
		Source:       "ses",
		Meta:         json.RawMessage(rawReq),
		CreatedAt:    time.Time(event.Mail.Timestamp),
	}, nil
}

func parseSendgridNotif(c echo.Context, app *App) ([]models.Bounce, error) {
	// Read the request body instead of using using c.Bind() to read to save the entire raw request as meta.
	rawReq, err := ioutil.ReadAll(c.Request().Body)
	if err != nil {
		app.log.Printf("error reading sendgrid notification body: %v", err)
		return nil, echo.NewHTTPError(http.StatusBadRequest, app.i18n.Ts("globals.messages.internalError"))
	}

	var events []sendgridNotif
	if err := json.Unmarshal(rawReq, &events); err != nil {
		app.log.Printf("error parsing sendgrid notification: %v", err)
		return nil, echo.NewHTTPError(http.StatusBadRequest, app.i18n.Ts("globals.messages.invalidData"))
	}

	var out []models.Bounce
	for _, e := range events {
		if e.Event != "bounce" {
			continue
		}

		tstamp := time.Unix(e.Timestamp, 0)
		b := models.Bounce{
			Email:     e.Email,
			Type:      models.BounceTypeHard,
			Source:    "sendgrid",
			CreatedAt: tstamp,
		}
		out = append(out, b)
	}

	return out, nil
}

func (st *sesTimestamp) UnmarshalJSON(b []byte) error {
	t, err := time.Parse("2006-01-02T15:04:05.999999999Z", strings.Trim(string(b), `"`))
	if err != nil {
		return err
	}
	*st = sesTimestamp(t)
	return nil
}
