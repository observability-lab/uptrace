package tracing

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/uptrace/bunrouter"
	"github.com/uptrace/go-clickhouse/ch"
	"github.com/uptrace/uptrace/pkg/bunapp"
	"github.com/uptrace/uptrace/pkg/bunutil"
	"github.com/uptrace/uptrace/pkg/httputil"
	"github.com/uptrace/uptrace/pkg/org"
	"github.com/uptrace/uptrace/pkg/tracing/attrkey"
)

type SystemHandler struct {
	*bunapp.App
}

func NewSystemHandler(app *bunapp.App) *SystemHandler {
	return &SystemHandler{
		App: app,
	}
}

func (h *SystemHandler) List(w http.ResponseWriter, req bunrouter.Request) error {
	ctx := req.Context()

	f, err := DecodeSystemFilter(h.App, req)
	if err != nil {
		return err
	}

	tableName := spanSystemTableForWhere(h.App, &f.TimeFilter)
	minutes := f.TimeFilter.Duration().Minutes()
	systems := make([]map[string]any, 0)

	if err := h.CH.NewSelect().
		ColumnExpr("system").
		ColumnExpr("sum(count) AS count").
		ColumnExpr("sum(count) / ? AS rate", minutes).
		ColumnExpr("sum(error_count) AS errorCount").
		ColumnExpr("sum(error_count) / sum(count) AS errorPct").
		TableExpr("?", tableName).
		WithQuery(f.whereClause).
		GroupExpr("system").
		OrderExpr("system ASC").
		Limit(1000).
		Scan(ctx, &systems); err != nil {
		return err
	}

	for _, sys := range systems {
		sys["projectId"] = f.ProjectID
		sys["isEvent"] = isEventSystem(sys["system"].(string))
	}

	return httputil.JSON(w, bunrouter.H{
		"systems": systems,
	})
}

func (h *SystemHandler) Stats(w http.ResponseWriter, req bunrouter.Request) error {
	ctx := req.Context()

	f, err := DecodeSystemFilter(h.App, req)
	if err != nil {
		return err
	}

	tableName, groupPeriod := spanSystemTableForGroup(h.App, &f.TimeFilter)

	subq := h.CH.NewSelect().
		WithAlias("tdigest_state", "quantilesTDigestWeightedMergeState(0.5, 0.9, 0.99, 1)(tdigest)").
		WithAlias("qsNaN", "finalizeAggregation(tdigest_state)").
		WithAlias("qs", "if(isNaN(qsNaN[1]), [0, 0, 0, 0], qsNaN)").
		ColumnExpr("system").
		ColumnExpr("sum(count) AS stats__count").
		ColumnExpr("sum(count) / ? AS stats__rate", groupPeriod.Minutes()).
		ColumnExpr("sum(error_count) AS stats__errorCount").
		ColumnExpr("sum(error_count) / sum(count) AS stats__errorPct").
		ColumnExpr("tdigest_state").
		ColumnExpr("qs[1] AS stats__durationP50").
		ColumnExpr("qs[2] AS stats__durationP90").
		ColumnExpr("qs[3] AS stats__durationP99").
		ColumnExpr("qs[4] AS stats__durationMax").
		ColumnExpr("toStartOfInterval(time, INTERVAL ? minute) AS time_", groupPeriod.Minutes()).
		TableExpr("?", tableName).
		Where("system != ?", SystemInternalSpan).
		WithQuery(f.whereClause).
		GroupExpr("system, time_").
		OrderExpr("system ASC, time_ ASC").
		Limit(10000)

	systems := make([]map[string]any, 0)

	if err := h.CH.NewSelect().
		WithAlias("qsNaN", "quantilesTDigestWeightedMerge(0.5, 0.9, 0.99)(tdigest_state)").
		WithAlias("qs", "if(isNaN(qsNaN[1]), [0, 0, 0], qsNaN)").
		ColumnExpr("system").
		ColumnExpr("sum(stats__count) AS count").
		ColumnExpr("sum(stats__count) / ? AS rate", f.Duration().Minutes()).
		ColumnExpr("sum(stats__errorCount) AS errorCount").
		ColumnExpr("sum(stats__errorCount) / sum(stats__count) AS errorPct").
		ColumnExpr("qs[1] AS durationP50").
		ColumnExpr("qs[2] AS durationP90").
		ColumnExpr("qs[3] AS durationP99").
		ColumnExpr("max(stats__durationMax) AS durationMax").
		ColumnExpr("groupArray(stats__count) AS stats__count").
		ColumnExpr("groupArray(stats__rate) AS stats__rate").
		ColumnExpr("groupArray(stats__errorCount) AS stats__errorCount").
		ColumnExpr("groupArray(stats__errorPct) AS stats__errorPct").
		ColumnExpr("groupArray(stats__durationP50) AS stats__durationP50").
		ColumnExpr("groupArray(stats__durationP90) AS stats__durationP90").
		ColumnExpr("groupArray(stats__durationP99) AS stats__durationP99").
		ColumnExpr("groupArray(stats__durationMax) AS stats__durationMax").
		ColumnExpr("groupArray(time_) AS stats__time").
		TableExpr("(?)", subq).
		GroupExpr("system").
		OrderExpr("system ASC").
		Limit(1000).
		Scan(ctx, &systems); err != nil {
		return err
	}

	for _, sys := range systems {
		isEvent := isEventSystem(sys["system"].(string))
		sys["isEvent"] = isEvent

		stats := sys["stats"].(map[string]any)
		if isEvent {
			delete(stats, "errorCount")
			delete(stats, "errorPct")
			delete(stats, "durationP50")
			delete(stats, "durationP90")
			delete(stats, "durationP99")
			delete(stats, "durationMax")
		}
		bunutil.FillHoles(stats, f.TimeGTE, f.TimeLT, groupPeriod)
	}

	return httputil.JSON(w, bunrouter.H{
		"systems": systems,
	})
}

//------------------------------------------------------------------------------

func (h *SystemHandler) Overview(w http.ResponseWriter, req bunrouter.Request) error {
	ctx := req.Context()

	f, err := DecodeSystemFilter(h.App, req)
	if err != nil {
		return err
	}

	attrKey := req.Form.Get("attr")
	if attrKey == "" {
		return errors.New(`"attr" query param is required`)
	}

	var groups []map[string]any

	switch attrKey {
	case attrkey.ServiceName:
		groups, err = h.selectServicesGroups(ctx, f)
		if err != nil {
			return err
		}
	case attrkey.HostName:
		groups, err = h.selectHosts(ctx, f)
		if err != nil {
			return err
		}
	default:
		groups, err = h.selectGroups(ctx, f, attrKey)
		if err != nil {
			return err
		}
	}

	return httputil.JSON(w, bunrouter.H{
		"groups": groups,
	})
}

//------------------------------------------------------------------------------

func (h *SystemHandler) selectServicesGroups(
	ctx context.Context, f *SystemFilter,
) ([]map[string]any, error) {
	tableName, groupPeriod := spanServiceTableInterval(h.App, &f.TimeFilter, org.CompactGroupPeriod)
	isEvent := isEventSystem(f.System)

	subq := h.CH.NewSelect().
		ColumnExpr("service AS attr").
		ColumnExpr("sum(count) AS stats__count").
		ColumnExpr("sum(count) / ? AS stats__rate", groupPeriod.Minutes()).
		ColumnExpr("toStartOfInterval(time, INTERVAL ? minute) AS time_",
			groupPeriod.Minutes()).
		TableExpr("?", tableName).
		WithQuery(f.whereClause).
		GroupExpr("attr, time_").
		OrderExpr("attr, time_ ASC").
		Limit(10000)

	if !isEvent {
		subq = subq.WithAlias("dur_tdigest_state",
			"quantilesTDigestWeightedMergeState(0.5, 0.9, 0.99, 1)(tdigest)").
			WithAlias("qsNaN", "finalizeAggregation(dur_tdigest_state)").
			WithAlias("qs", "if(isNaN(qsNaN[1]), [0, 0, 0], qsNaN)").
			ColumnExpr("sum(error_count) AS stats__errorCount").
			ColumnExpr("sum(error_count) / sum(count) AS stats__errorPct").
			ColumnExpr("dur_tdigest_state").
			ColumnExpr("round(qs[1]) AS stats__durationP50").
			ColumnExpr("round(qs[2]) AS stats__durationP90").
			ColumnExpr("round(qs[3]) AS stats__durationP99").
			ColumnExpr("round(qs[4]) AS stats__durationMax")
	}

	q := h.CH.NewSelect().
		ColumnExpr("attr").
		ColumnExpr("sum(stats__count) AS count").
		ColumnExpr("sum(stats__count) / ? AS rate", f.Duration().Minutes()).
		ColumnExpr("groupArray(stats__count) AS stats__count").
		ColumnExpr("groupArray(stats__rate) AS stats__rate").
		ColumnExpr("groupArray(time_) AS stats__time").
		TableExpr("(?) AS s", subq).
		Group("attr").
		Order("attr").
		Limit(1000)

	if !isEvent {
		q = q.WithAlias("qsNaN", "quantilesTDigestWeightedMerge(0.5, 0.9, 0.99)(dur_tdigest_state)").
			WithAlias("qs", "if(isNaN(qsNaN[1]), [0, 0, 0], qsNaN)").
			ColumnExpr("sum(stats__errorCount) AS errorCount").
			ColumnExpr("sum(stats__errorCount) / sum(stats__count) AS errorPct").
			ColumnExpr("round(qs[1]) AS durationP50").
			ColumnExpr("round(qs[2]) AS durationP90").
			ColumnExpr("round(qs[3]) AS durationP99").
			ColumnExpr("max(stats__durationMax) AS durationMax").
			ColumnExpr("groupArray(stats__errorCount) AS stats__errorCount").
			ColumnExpr("groupArray(stats__errorPct) AS stats__errorPct").
			ColumnExpr("groupArray(stats__durationP50) AS stats__durationP50").
			ColumnExpr("groupArray(stats__durationP90) AS stats__durationP90").
			ColumnExpr("groupArray(stats__durationP99) AS stats__durationP99").
			ColumnExpr("groupArray(stats__durationMax) AS stats__durationMax")
	}

	services := make([]map[string]any, 0)

	if err := q.Scan(ctx, &services); err != nil {
		return nil, err
	}

	for _, service := range services {
		stats := service["stats"].(map[string]any)
		bunutil.FillHoles(stats, f.TimeGTE, f.TimeLT, groupPeriod)
	}

	return services, nil
}

func spanServiceTableInterval(
	app *bunapp.App, f *org.TimeFilter, groupPeriodFn func(time.Time, time.Time) time.Duration,
) (ch.Ident, time.Duration) {
	tablePeriod, groupPeriod := org.TableGroupPeriod(f)
	switch tablePeriod {
	case time.Minute:
		return app.DistTable("span_service_minutes"), groupPeriod
	case time.Hour:
		return app.DistTable("span_service_hours"), groupPeriod
	}
	panic("not reached")
}

//------------------------------------------------------------------------------

func (h *SystemHandler) selectHosts(
	ctx context.Context, f *SystemFilter,
) ([]map[string]any, error) {
	tableName, groupPeriod := spanHostTableInterval(h.App, &f.TimeFilter, org.CompactGroupPeriod)
	isEvent := isEventSystem(f.System)

	subq := h.CH.NewSelect().
		ColumnExpr("host AS attr").
		ColumnExpr("sum(count) AS stats__count").
		ColumnExpr("sum(count) / ? AS stats__rate", groupPeriod.Minutes()).
		ColumnExpr("toStartOfInterval(time, INTERVAL ? minute) AS time_",
			groupPeriod.Minutes()).
		TableExpr("?", tableName).
		WithQuery(f.whereClause).
		GroupExpr("attr, time_").
		OrderExpr("attr, time_ ASC")

	if !isEvent {
		subq = subq.WithAlias("dur_tdigest_state",
			"quantilesTDigestWeightedMergeStateOrDefault(0.5, 0.9, 0.99, 1)(tdigest)").
			WithAlias("qsNaN", "finalizeAggregation(dur_tdigest_state)").
			WithAlias("qs", "if(isNaN(qsNaN[1]), [0, 0, 0], qsNaN)").
			ColumnExpr("sum(error_count) AS stats__errorCount").
			ColumnExpr("sum(error_count) / sum(count) AS stats__errorPct").
			ColumnExpr("dur_tdigest_state").
			ColumnExpr("round(qs[1]) AS stats__durationP50").
			ColumnExpr("round(qs[2]) AS stats__durationP90").
			ColumnExpr("round(qs[3]) AS stats__durationP99").
			ColumnExpr("round(qs[4]) AS stats__durationMax")
	}

	q := h.CH.NewSelect().
		ColumnExpr("attr").
		ColumnExpr("sum(stats__count) AS count").
		ColumnExpr("sum(stats__count) / ? AS rate", f.Duration().Minutes()).
		ColumnExpr("groupArray(stats__count) AS stats__count").
		ColumnExpr("groupArray(stats__rate) AS stats__rate").
		ColumnExpr("groupArray(time_) AS stats__time").
		TableExpr("(?) AS s", subq).
		Group("attr").
		Order("attr").
		Limit(1000)

	if !isEvent {
		q = q.WithAlias("qsNaN",
			"quantilesTDigestWeightedMergeOrDefault(0.5, 0.9, 0.99)(dur_tdigest_state)").
			WithAlias("qs", "if(isNaN(qsNaN[1]), [0, 0, 0], qsNaN)").
			ColumnExpr("sum(stats__errorCount) AS errorCount").
			ColumnExpr("sum(stats__errorCount) / sum(stats__count) AS errorPct").
			ColumnExpr("round(qs[1]) AS durationP50").
			ColumnExpr("round(qs[2]) AS durationP90").
			ColumnExpr("round(qs[3]) AS durationP99").
			ColumnExpr("max(stats__durationMax) AS durationMax").
			ColumnExpr("groupArray(stats__errorCount) AS stats__errorCount").
			ColumnExpr("groupArray(stats__errorPct) AS stats__errorPct").
			ColumnExpr("groupArray(stats__durationP50) AS stats__durationP50").
			ColumnExpr("groupArray(stats__durationP90) AS stats__durationP90").
			ColumnExpr("groupArray(stats__durationP99) AS stats__durationP99").
			ColumnExpr("groupArray(stats__durationMax) AS stats__durationMax")
	}

	hosts := make([]map[string]any, 0)

	if err := q.Scan(ctx, &hosts); err != nil {
		return nil, err
	}

	for _, host := range hosts {
		stats := host["stats"].(map[string]any)
		bunutil.FillHoles(stats, f.TimeGTE, f.TimeLT, groupPeriod)
	}

	return hosts, nil
}

func spanHostTableInterval(
	app *bunapp.App, f *org.TimeFilter, groupPeriodFn func(time.Time, time.Time) time.Duration,
) (ch.Ident, time.Duration) {
	tablePeriod, groupPeriod := org.TableGroupPeriod(f)
	switch tablePeriod {
	case time.Minute:
		return app.DistTable("span_host_minutes"), groupPeriod
	case time.Hour:
		return app.DistTable("span_host_hours"), groupPeriod
	}
	panic("not reached")
}

//------------------------------------------------------------------------------

func (h *SystemHandler) selectGroups(
	ctx context.Context, f *SystemFilter, attrKey string,
) ([]map[string]any, error) {
	groupPeriod := org.GroupPeriod(f.TimeGTE, f.TimeLT)
	isEvent := isEventSystem(f.System)

	subq := NewSpanIndexQuery(h.App).
		ColumnExpr("? AS attr", CHAttrExpr(attrKey)).
		ColumnExpr("sum(count) AS stats__count").
		ColumnExpr("sum(count) / ? AS stats__rate", groupPeriod.Minutes()).
		ColumnExpr("toStartOfInterval(time, INTERVAL ? minute) AS time_",
			groupPeriod.Minutes()).
		WithQuery(f.whereClause).
		GroupExpr("attr, time_").
		OrderExpr("attr, time_ ASC")

	if !isEvent {
		subq = subq.
			// no need to use sample factor here
			WithAlias(
				"dur_tdigest_state",
				"quantilesTDigestWeightedStateIf(0.5, 0.9, 0.99)(toFloat32(duration), toUInt32(count), duration > 0)",
			).
			WithAlias("qsNaN", "finalizeAggregation(dur_tdigest_state)").
			WithAlias("qs", "if(isNaN(qsNaN[1]), [0, 0, 0], qsNaN)").
			ColumnExpr("sumIf(count, status_code = 'error') AS stats__errorCount").
			ColumnExpr("sumIf(count, status_code = 'error') / sum(count) AS stats__errorPct").
			ColumnExpr("dur_tdigest_state").
			ColumnExpr("round(qs[1]) AS stats__durationP50").
			ColumnExpr("round(qs[2]) AS stats__durationP90").
			ColumnExpr("round(qs[3]) AS stats__durationP99").
			ColumnExpr("max(duration) AS stats__durationMax")
	}

	q := h.CH.NewSelect().
		ColumnExpr("attr").
		ColumnExpr("sum(stats__count) AS count").
		ColumnExpr("sum(stats__count) / ? AS rate", f.TimeFilter.Duration().Minutes()).
		ColumnExpr("groupArray(stats__count) AS stats__count").
		ColumnExpr("groupArray(stats__rate) AS stats__rate").
		ColumnExpr("groupArray(time_) AS stats__time").
		TableExpr("(?)", subq).
		Group("attr").
		Order("attr").
		Limit(1000)

	if !isEvent {
		q = q.
			WithAlias(
				"qsNaN",
				"quantilesTDigestWeightedMergeOrDefault(0.5, 0.9, 0.99)(dur_tdigest_state)",
			).
			WithAlias("qs", "if(isNaN(qsNaN[1]), [0, 0, 0], qsNaN)").
			ColumnExpr("sum(stats__errorCount) AS errorCount").
			ColumnExpr("sum(stats__errorCount) / sum(stats__count) AS errorPct").
			ColumnExpr("round(qs[1]) AS durationP50").
			ColumnExpr("round(qs[2]) AS durationP90").
			ColumnExpr("round(qs[3]) AS durationP99").
			ColumnExpr("max(stats__durationMax) AS durationMax").
			ColumnExpr("groupArray(stats__errorCount) AS stats__errorCount").
			ColumnExpr("groupArray(stats__errorPct) AS stats__errorPct").
			ColumnExpr("groupArray(stats__durationP50) AS stats__durationP50").
			ColumnExpr("groupArray(stats__durationP90) AS stats__durationP90").
			ColumnExpr("groupArray(stats__durationP99) AS stats__durationP99").
			ColumnExpr("groupArray(stats__durationMax) AS stats__durationMax")
	}

	groups := make([]map[string]any, 0)

	if err := q.Scan(ctx, &groups); err != nil {
		return nil, err
	}

	for _, group := range groups {
		stats := group["stats"].(map[string]any)
		bunutil.FillHoles(stats, f.TimeGTE, f.TimeLT, groupPeriod)
	}

	return groups, nil
}

//------------------------------------------------------------------------------

func (h *SystemHandler) ListEnvs(w http.ResponseWriter, req bunrouter.Request) error {
	ctx := req.Context()

	f, err := DecodeSystemFilter(h.App, req)
	if err != nil {
		return err
	}

	envs, err := h.selectEnvs(ctx, f)
	if err != nil {
		return err
	}

	return httputil.JSON(w, bunrouter.H{
		"items": envs,
	})
}

func (h *SystemHandler) selectEnvs(ctx context.Context, f *SystemFilter) ([]string, error) {
	var envs []string

	tableName := spanSystemTableForWhere(h.App, &f.TimeFilter)
	if err := h.CH.NewSelect().
		ColumnExpr("DISTINCT deployment.environment").
		TableExpr("? AS s", tableName).
		WithQuery(f.whereClause).
		Where("notEmpty(deployment.environment)").
		OrderExpr("deployment.environment ASC").
		Limit(100).
		ScanColumns(ctx, &envs); err != nil {
		return nil, err
	}

	return envs, nil
}

//------------------------------------------------------------------------------

func (h *SystemHandler) ListServices(w http.ResponseWriter, req bunrouter.Request) error {
	ctx := req.Context()

	f, err := DecodeSystemFilter(h.App, req)
	if err != nil {
		return err
	}

	services, err := h.selectServices(ctx, f)
	if err != nil {
		return err
	}

	return httputil.JSON(w, bunrouter.H{
		"items": services,
	})
}

func (h *SystemHandler) selectServices(ctx context.Context, f *SystemFilter) ([]string, error) {
	var services []string

	tableName := spanSystemTableForWhere(h.App, &f.TimeFilter)
	if err := h.CH.NewSelect().
		ColumnExpr("DISTINCT service").
		TableExpr("? AS s", tableName).
		WithQuery(f.whereClause).
		Where("notEmpty(service)").
		OrderExpr("service ASC").
		Limit(100).
		ScanColumns(ctx, &services); err != nil {
		return nil, err
	}

	return services, nil
}
