// This file holds the names of the settings (environment variables) the
// program reads. Names still written as literals elsewhere are being moved
// here over time.

package main

// databaseURLSetting names SCOUT_DATABASE_URL in errors, which never quote
// its value (see database.ConnStringError).
const databaseURLSetting = "SCOUT_DATABASE_URL"
