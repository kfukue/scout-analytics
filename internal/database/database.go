package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	pgx5 "github.com/jackc/pgx/v5"
	pgxpool5 "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	"github.com/kfukue/lyle-labs-libraries/v2/utils"
	_ "github.com/lib/pq"
)

const (
	port        = 5432
	dbname      = "assetdb"
	sslmode     = "verify-ca"
	sslrootcert = `server-ca.pem`
	sslcert     = `client-cert.pem`
	sslkey      = `client-key.pem`
)

var DbConn *sql.DB
var DbConnPgx *pgxpool5.Pool
var DbConnV5 *pgx5.Conn

// func init() {
// 	DbConn, DbConnPgx, _ = SetupDatabase()
// }

// SetupDatabase
func SetupDatabase() (*sql.DB, *pgxpool5.Pool, error) {
	godotenv.Load()
	var err error
	dbURI := "host=%s port=%d user=%s password=%s dbname=%s sslmode=%s  sslrootcert=%s sslcert=%s sslkey=%s"
	socketDir, isSet := os.LookupEnv("DB_SOCKET_DIR")
	if !isSet {
		socketDir = "/cloudsql"
	}
	if utils.GetEnv() == "production" {
		log.Println("In production!")
		dbSecretPath := utils.MustGetenv("DB_SECRET_PATH")
		// Set each value dynamically w/ Sprintf
		password, err := utils.AccessSecretVersion(dbSecretPath)
		if err != nil {
			log.Fatal(err)
		}
		userSecretPath := utils.MustGetenv("USER_SECRET_PATH")
		user, err := utils.AccessSecretVersion(userSecretPath)
		if err != nil {
			log.Fatal(err)
		}
		instanceSecretPath := utils.MustGetenv("INSTANCE_SECRET_PATH")
		instanceConnectionName, err := utils.AccessSecretVersion(instanceSecretPath)
		if err != nil {
			log.Fatal(err)
		}
		dbURI = fmt.Sprintf("user=%s password=%s database=%s host=%s/%s", user, password, dbname, socketDir, instanceConnectionName)
		if err != nil {
			log.Fatal(fmt.Errorf("sql.Open: %v", err))
		}
	} else {
		dir := utils.GoDotEnvVariable("SSL_CERT_FILE_PATH")
		filepath.Abs(dir)
		fmt.Println("in base : " + dir)
		user := utils.GoDotEnvVariable("DB_USER")
		password := utils.GoDotEnvVariable("DB_PASS")
		dbname := utils.GoDotEnvVariable("DB_NAME_DEV")
		hostSecretPath := utils.GoDotEnvVariable("HOST_SECRET_PATH")
		var host string
		if utils.GetEnv() == "LOCAL_GETH" {
			host = utils.GoDotEnvVariable("GETH_HOST_PATH")
			dbURI = "host=%s port=%d user=%s password=%s dbname=%s"
			dbURI = fmt.Sprintf(dbURI, host, port, user, password, dbname)
		} else {
			host, err = utils.AccessSecretVersion(hostSecretPath)
			if err != nil {
				log.Fatal(err)
			}
			dbURI = fmt.Sprintf(dbURI, host, port, user, password, dbname, sslmode, filepath.Join(dir, sslrootcert), filepath.Join(dir, sslcert), filepath.Join(dir, sslkey))
		}
	}
	DbConn, err := sql.Open("pgx", dbURI)
	// DbConn, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal(err)
	}

	if err != nil {
		log.Fatal(err)
	}
	err = DbConn.Ping()
	if err != nil {
		panic(err)
	}
	DbConn.SetMaxOpenConns(5000)
	DbConn.SetMaxIdleConns(5000)
	DbConn.SetConnMaxLifetime(600 * time.Second)
	config5, err := pgxpool5.ParseConfig(dbURI)
	if err != nil {
		log.Fatal(err)
	}
	config5.AfterConnect = func(ctx context.Context, conn *pgx5.Conn) error {
		pgxdecimal.Register(conn.TypeMap())
		return nil
	}
	db, _ := pgx5.Connect(context.Background(), dbURI)
	DbConnV5 = db
	DbConnPgx, err = pgxpool5.NewWithConfig(context.Background(), config5)
	return DbConn, DbConnPgx, nil

}
