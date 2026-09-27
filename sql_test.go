package otel

import (
	"testing"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// The driver name a service registers is not necessarily the system name the
// dashboards group by - Fileshare registers its wrapped driver as "mysql-mw" -
// so the mapping is asserted rather than assumed.
func TestDBSystemMapsDriverNames(t *testing.T) {
	cases := map[string]string{
		"mysql":    string(semconv.DBSystemNameMySQL.Value.AsString()),
		"mysql-mw": string(semconv.DBSystemNameMySQL.Value.AsString()),
		"postgres": string(semconv.DBSystemNamePostgreSQL.Value.AsString()),
		"pgx":      string(semconv.DBSystemNamePostgreSQL.Value.AsString()),
		"mariadb":  string(semconv.DBSystemNameMariaDB.Value.AsString()),
	}

	for driver, want := range cases {
		got := dbSystem(driver).Value.AsString()
		if got != want {
			t.Errorf("dbSystem(%q) = %q, want %q", driver, got, want)
		}
	}
}

// An unknown driver still reports something rather than dropping the
// attribute, which would leave the span unattributable.
func TestDBSystemFallsBackToTheDriverName(t *testing.T) {
	if got := dbSystem("clickhouse").Value.AsString(); got != "clickhouse" {
		t.Errorf("dbSystem fallback = %q, want %q", got, "clickhouse")
	}
}

func TestDBAttributesIncludeNamespace(t *testing.T) {
	attrs := dbAttributes("mysql", "urlshortener")

	var namespace string
	for _, a := range attrs {
		if a.Key == semconv.DBNamespaceKey {
			namespace = a.Value.AsString()
		}
	}

	if namespace != "urlshortener" {
		t.Errorf("db.namespace = %q, want %q", namespace, "urlshortener")
	}
}

// A service with no database name should not emit an empty db.namespace,
// which reads on a dashboard as "a database called nothing".
func TestDBAttributesOmitEmptyNamespace(t *testing.T) {
	for _, a := range dbAttributes("mysql", "") {
		if a.Key == semconv.DBNamespaceKey {
			t.Error("db.namespace was set despite an empty namespace")
		}
	}
}
