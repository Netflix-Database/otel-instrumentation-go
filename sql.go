package otel

import (
	"database/sql"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.uber.org/zap"
)

// OpenDB opens a database whose queries are recorded as spans.
//
// Go has no equivalent of the runtime patching the Node and .NET SDKs use, so
// a database is only traced if it is opened through an instrumented driver.
// `sql.Open` produces no spans at all, silently - which is why these services
// reported http spans and nothing from the database.
//
// The attributes match what the Node and .NET services emit, so one dashboard
// panel covers every language: db.system.name, db.namespace and db.query.text
// under semantic convention 1.43.0.
//
// Query text is recorded; bound parameters are not, and otelsql never captures
// them. A query's values are exactly where credentials and personal data live,
// and spans are not a place for either.
func OpenDB(driverName, dsn, namespace string) (*sql.DB, error) {
	attrs := dbAttributes(driverName, namespace)

	db, err := otelsql.Open(driverName, dsn,
		otelsql.WithAttributes(attrs...),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			// Health probes ping constantly; a span each would swamp both the
			// trace volume and the request-rate panels.
			Ping: false,
			// One event per row turns a single result set into thousands.
			RowsNext: false,
			// driver.ErrSkip is control flow - the driver declining an
			// optional fast path - not a failure worth marking a span with.
			DisableErrSkip: true,
		}),
	)
	if err != nil {
		return nil, err
	}

	// Pool saturation is the thing you want on the dashboard when queries start
	// queueing, and it is not visible from spans. Losing it is not a reason to
	// refuse to serve, so a failure here is reported and the database returned
	// anyway.
	if _, err := otelsql.RegisterDBStatsMetrics(db, otelsql.WithAttributes(attrs...)); err != nil {
		CreateLogger("otel").Warn("Could not register database pool metrics", zap.Error(err))
	}

	return db, nil
}

// dbAttributes describes the database every span and metric is tagged with.
func dbAttributes(driverName, namespace string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{dbSystem(driverName)}

	if namespace != "" {
		attrs = append(attrs, semconv.DBNamespace(namespace))
	}

	return attrs
}

// dbSystem maps a driver name onto the semantic convention's system name, so
// "mysql" and "postgres" are reported the way the dashboards expect rather
// than as whatever the driver happens to be registered as.
func dbSystem(driverName string) attribute.KeyValue {
	switch driverName {
	case "mysql", "mysql-mw":
		return semconv.DBSystemNameMySQL
	case "postgres", "pgx":
		return semconv.DBSystemNamePostgreSQL
	case "mariadb":
		return semconv.DBSystemNameMariaDB
	default:
		return semconv.DBSystemNameKey.String(driverName)
	}
}
