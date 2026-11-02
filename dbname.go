package tracepanel

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// The database's name, db.namespace: what the DSN or the driver's config connects to ("shop"), not the alias the app
// knows the database by ("default"). Without one, a span has none.

// dsnName is the database a DSN names, read as its database's driver reads it: "" when it names none.
func dsnName(system, dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	switch system {
	case "sqlite", "duckdb":
		return fileName(dsn)
	}
	if strings.Contains(dsn, "://") {
		if parsed, err := url.Parse(dsn); err == nil {
			return urlName(system, parsed)
		}
	}
	switch system {
	case "postgresql", "redshift":
		// lib/pq's and pgx's keyword form: host=localhost dbname=shop user=app.
		return postgresName(keywords(strings.Fields(dsn)))
	case "mssql":
		// ADO's: server=localhost;database=shop.
		pairs := keywords(strings.Split(dsn, ";"))
		return first(pairs["database"], pairs["initial catalog"])
	case "mysql", "mariadb":
		return mysqlName(dsn)
	case "snowflake":
		// user:password@account/database/schema?warehouse=wh
		rest := dsn[strings.LastIndex(dsn, "@")+1:]
		rest, _, _ = strings.Cut(rest, "?")
		if parts := strings.Split(rest, "/"); len(parts) > 1 {
			return parts[1]
		}
	}
	return ""
}

// urlName is the database a URL DSN names: its database (or dbname) parameter, else its path's first part; SQL
// Server's path is an instance, and only the parameter names the database.
func urlName(system string, dsn *url.URL) string {
	query := dsn.Query()
	if name := first(query.Get("database"), query.Get("dbname")); name != "" || system == "mssql" {
		return name
	}
	path, _, _ := strings.Cut(strings.TrimPrefix(dsn.Path, "/"), "/")
	if system == "postgresql" || system == "redshift" {
		user := ""
		if dsn.User != nil {
			user = dsn.User.Username()
		}
		return postgresName(map[string]string{"dbname": path, "user": user})
	}
	return path
}

// postgresName is the database PostgreSQL connects to: the one named, PGDATABASE, else the user's own.
func postgresName(pairs map[string]string) string {
	return first(pairs["dbname"], os.Getenv("PGDATABASE"), pairs["user"], os.Getenv("PGUSER"))
}

// mysqlName is the database of a go-sql-driver/mysql DSN, user:password@tcp(host:3306)/shop?parseTime=true: after
// the last slash, as the driver reads it.
func mysqlName(dsn string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return ""
	}
	name, _, _ := strings.Cut(dsn[slash+1:], "?")
	return name
}

// fileName is a file database's name, its file's: none for one in memory.
func fileName(dsn string) string {
	path, query, _ := strings.Cut(strings.TrimPrefix(dsn, "file:"), "?")
	if path == "" || path == ":memory:" || strings.Contains(query, "mode=memory") {
		return ""
	}
	return filepath.Base(path)
}

// keywords are key=value pairs by their lowercase key, a value's quotes dropped.
func keywords(pairs []string) map[string]string {
	found := map[string]string{}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			found[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(value), `'"`)
		}
	}
	return found
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// configName is the database a driver's connector or an ORM's dialector is configured for: a field named Database
// or DBName, a dbname entry (lib/pq's), or a DSN field read by dsnName. pgx's stdlib connector, go-sql-driver/mysql's,
// lib/pq's, go-mssqldb's, GORM's dialectors.
func configName(system string, config any) string {
	return fieldName(system, reflect.ValueOf(config), 0)
}

func fieldName(system string, value reflect.Value, depth int) string {
	for value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Map:
		keyType := value.Type().Key()
		if keyType.Kind() != reflect.String {
			return ""
		}
		for _, key := range []string{"dbname", "database"} {
			item := value.MapIndex(reflect.ValueOf(key).Convert(keyType))
			if item.IsValid() && item.Kind() == reflect.String && item.String() != "" {
				return item.String()
			}
		}
	case reflect.Struct:
		for i := range value.NumField() {
			field := value.Field(i)
			if field.Kind() != reflect.String || field.String() == "" {
				continue
			}
			switch strings.ToLower(value.Type().Field(i).Name) {
			case "database", "dbname":
				return field.String()
			case "dsn":
				if name := dsnName(system, field.String()); name != "" {
					return name
				}
			}
		}
		// A config's own config: pgx's ConnConfig in its connector, pgconn's Config in that; a pgxpool's, under
		// stdlib.OpenDBFromPool's connector.
		if depth >= 5 {
			return ""
		}
		for i := range value.NumField() {
			switch field := value.Field(i); field.Kind() {
			case reflect.Struct, reflect.Pointer, reflect.Map:
				if name := fieldName(system, field, depth+1); name != "" {
					return name
				}
			}
		}
	}
	return ""
}
