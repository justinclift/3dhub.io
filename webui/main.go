package main

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sqlite "github.com/gwenn/gosqlite"
	"github.com/icza/session"
	com "github.com/sqlitebrowser/dbhub.io/common"
	"golang.org/x/oauth2"
)

var (
	// Log file for incoming HTTPS requests
	reqLog *os.File

	// Our parsed HTML templates
	tmpl *template.Template
)

// auth0CallbackHandler is called at the end of the Auth0 authentication process, whether successful or not.
// If the authentication process was successful:
//  * if the user already has an account on our system then this function creates a login session for them.
//  * if the user doesn't yet have an account on our system, they're bounced to the username selection page.
// If the authentication process wasn't successful, an error message is displayed.
func auth0CallbackHandler(w http.ResponseWriter, r *http.Request) {
	// Auth0 login part, mostly copied from https://github.com/auth0-samples/auth0-golang-web-app (MIT License)
	conf := &oauth2.Config{
		ClientID:     com.Auth0ClientID(),
		ClientSecret: com.Auth0ClientSecret(),
		RedirectURL:  "https://" + com.WebServer() + "/x/callback",
		Scopes:       []string{"openid", "profile"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://" + com.Auth0Domain() + "/authorize",
			TokenURL: "https://" + com.Auth0Domain() + "/oauth/token",
		},
	}
	code := r.URL.Query().Get("code")
	token, err := conf.Exchange(oauth2.NoContext, code)
	if err != nil {
		log.Printf("Login failure: %s\n", err.Error())
		errorPage(w, r, http.StatusInternalServerError, "Login failed")
		return
	}

	// Retrieve the user info (JSON format)
	conn := conf.Client(oauth2.NoContext, token)
	userInfo, err := conn.Get("https://" + com.Auth0Domain() + "/userinfo")
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := ioutil.ReadAll(userInfo.Body)
	defer userInfo.Body.Close()
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Convert the JSON into something usable
	var profile map[string]interface{}
	if err = json.Unmarshal(raw, &profile); err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Determine the DBHub.io username matching the given Auth0 ID
	email := profile["email"].(string)
	auth0ID := profile["user_id"].(string)
	userName, err := com.UserNameFromAuth0ID(auth0ID)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// If the user doesn't already exist, we need to create an account for them
	if userName == "" {
		// Check if the email address is already in our system
		exists, err := com.CheckEmailExists(email)
		if err != nil {
			errorPage(w, r, http.StatusInternalServerError, "Email check failed")
			return
		}
		if exists {
			errorPage(w, r, http.StatusConflict,
				"Can't create new account: Your email address is already associated with a "+
					"different account in our system.")
			return
		}

		// Create a special session cookie, purely for the registration page
		sess := session.NewSessionOptions(&session.SessOptions{
			CAttrs: map[string]interface{}{
				"registrationinprogress": true,
				"auth0id":                auth0ID,
				"email":                  email},
		})
		session.Add(sess, w)

		// Bounce to a new page, for the user to select their preferred username
		http.Redirect(w, r, "/selectusername", http.StatusTemporaryRedirect)
	}

	// Create session cookie for the user
	sess := session.NewSessionOptions(&session.SessOptions{
		CAttrs: map[string]interface{}{"UserName": userName},
	})
	session.Add(sess, w)

	// Login completed, so bounce to the user's profile page
	http.Redirect(w, r, "/"+userName, http.StatusTemporaryRedirect)
}

func createUserHandler(w http.ResponseWriter, r *http.Request) {
	// Make sure this user creation session is valid
	sess := session.Get(r)
	if sess == nil {
		// This isn't a valid username selection session, so abort
		errorPage(w, r, http.StatusBadRequest, "Invalid user creation session")
		return
	}

	validRegSession := false
	va := sess.CAttr("registrationinprogress")
	if va == nil {
		// This isn't a valid username selection session, so abort
		errorPage(w, r, http.StatusBadRequest, "Invalid user creation session")
		return
	}
	validRegSession = va.(bool)

	if validRegSession != true {
		// This isn't a valid username selection session, so abort
		errorPage(w, r, http.StatusBadRequest, "Invalid user creation session")
		return
	}

	// Retrieve the registration data
	var auth0ID, email string
	au := sess.CAttr("auth0id")
	if au != nil {
		auth0ID = au.(string)
	} else {
		errorPage(w, r, http.StatusBadRequest, "Invalid user creation id")
		return
	}
	em := sess.CAttr("email")
	if em != nil {
		email = em.(string)
	} else {
		errorPage(w, r, http.StatusBadRequest, "Invalid user creation email")
		return
	}

	// Gather submitted form data (if any)
	err := r.ParseForm()
	if err != nil {
		log.Printf("Error when parsing user creation data: %s\n", err)
		errorPage(w, r, http.StatusBadRequest, "Error when parsing user creation data")
		return
	}
	userName := r.PostFormValue("username")

	// Ensure username was given
	if userName == "" {
		// No, so render the username selection page
		selectUsernamePage(w, r)
		return
	}

	// Validate the user supplied username
	err = com.ValidateUser(userName)
	if err != nil {
		log.Printf("Username failed validation: %s", err)
		session.Remove(sess, w)
		errorPage(w, r, http.StatusBadRequest, "Username failed validation")
		return
	}

	// Ensure the username isn't a reserved one
	err = com.ReservedUsernamesCheck(userName)
	if err != nil {
		log.Println(err)
		session.Remove(sess, w)
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Check if the username is already in our system
	exists, err := com.CheckUserExists(userName)
	if err != nil {
		session.Remove(sess, w)
		errorPage(w, r, http.StatusInternalServerError, "Username check failed")
		return
	}
	if exists {
		session.Remove(sess, w)
		errorPage(w, r, http.StatusConflict, "That username is already taken")
		return
	}

	// Add the user to the system
	// NOTE: We generate a random password here (for now).  We may remove the password field itself from the
	// database at some point, depending on whether we continue to support local database users
	err = com.AddUser(auth0ID, userName, com.RandomString(32), email)
	if err != nil {
		session.Remove(sess, w)
		errorPage(w, r, http.StatusInternalServerError, "Something went wrong during user creation")
		return
	}

	// Remove the temporary username selection session data
	session.Remove(sess, w)

	// Create normal session cookie for the user
	// TODO: This will probably leak a small amount of memory, but it's "good enough" for now while getting things
	// working
	sess = session.NewSessionOptions(&session.SessOptions{
		CAttrs: map[string]interface{}{"UserName": userName},
	})
	session.Add(sess, w)

	// User creation completed, so bounce to the user's profile page
	http.Redirect(w, r, "/"+userName, http.StatusTemporaryRedirect)
}

func downloadCSVHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Download CSV"

	// Extract the username, database, table, and version requested
	dbOwner, dbName, dbTable, dbVersion, err := com.GetODTV(2, r) // 2 = Ignore "/x/download/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Abort if no table name was given
	if dbTable == "" {
		log.Printf("%s: No table name given\n", pageName)
		errorPage(w, r, http.StatusBadRequest, "No table name given")
		return
	}

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		u := sess.CAttr("UserName")
		if u != nil {
			loggedInUser = u.(string)
		} else {
			session.Remove(sess, w)
		}
	}

	// Verify the given database exists and is ok to be downloaded (and get the Minio bucket + id while at it)
	bucket, id, err := com.MinioBucketID(dbOwner, dbName, int(dbVersion), loggedInUser)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Get a handle from Minio for the database object
	sdb, err := com.OpenMinioObject(bucket, id)
	if err != nil {
		log.Printf("%s: Error retrieving DB from Minio: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}

	// Read the table data from the database object
	resultSet, err := com.ReadSQLiteDBCSV(sdb, dbTable)

	// Convert resultSet into CSV and send to the user
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.csv", url.QueryEscape(dbTable)))
	w.Header().Set("Content-Type", "text/csv")
	csvFile := csv.NewWriter(w)
	err = csvFile.WriteAll(resultSet)
	if err != nil {
		log.Printf("%s: Error when generating CSV: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Error when generating CSV")
		return
	}
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Download Handler"

	dbOwner, dbName, dbVersion, err := com.GetODV(2, r) // 2 = Ignore "/x/download/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		u := sess.CAttr("UserName")
		if u != nil {
			loggedInUser = u.(string)
		} else {
			session.Remove(sess, w)
		}
	}

	// Verify the given database exists and is ok to be downloaded (and get the Minio bucket + id while at it)
	bucket, id, err := com.MinioBucketID(dbOwner, dbName, dbVersion, loggedInUser)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Get a handle from Minio for the database object
	userDB, err := com.MinioHandle(bucket, id)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Close the object handle when this function finishes
	defer func() {
		com.MinioHandleClose(userDB)
	}()

	// Send the database to the user
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", url.QueryEscape(dbName)))
	w.Header().Set("Content-Type", "application/x-sqlite3")
	bytesWritten, err := io.Copy(w, userDB)
	if err != nil {
		log.Printf("%s: Error returning DB file: %v\n", pageName, err)
		fmt.Fprintf(w, "%s: Error returning DB file: %v\n", pageName, err)
		return
	}

	// Log the number of bytes written
	log.Printf("%s: '%s/%s' downloaded. %d bytes", pageName, dbOwner, dbName, bytesWritten)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	// Remove session info
	sess := session.Get(r)
	if sess != nil {
		// Session data was present, so remove it
		session.Remove(sess, w)
	}

	// Bounce to the front page
	// TODO: This should probably reload the existing page instead
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

// Wrapper function to log incoming https requests
func logReq(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if user is logged in
		var loggedInUser string
		sess := session.Get(r)
		if sess != nil {
			u := sess.CAttr("UserName")
			if u != nil {
				loggedInUser = u.(string)
			} else {
				loggedInUser = "-"
			}
		} else {
			loggedInUser = "-"
		}

		// Write request details to the request log
		fmt.Fprintf(reqLog, "%v - %s [%s] \"%s %s %s\" \"-\" \"-\" \"%s\" \"%s\"\n", r.RemoteAddr,
			loggedInUser, time.Now().Format(time.RFC3339Nano), r.Method, r.URL, r.Proto,
			r.Referer(), r.Header.Get("User-Agent"))

		// Call the original function
		fn(w, r)
	}
}

func main() {
	// Read server configuration
	var err error
	if err = com.ReadConfig(); err != nil {
		log.Fatalf("Configuration file problem\n\n%v", err)
	}

	// Open the request log for writing
	reqLog, err = os.OpenFile(com.WebRequestLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0750)
	if err != nil {
		log.Fatalf("Error when opening request log: %s\n", err)
	}
	defer reqLog.Close()
	log.Printf("Request log opened: %s\n", com.WebRequestLog())

	// Setup session storage
	session.Global.Close()
	session.Global = session.NewCookieManagerOptions(session.NewInMemStore(),
		&session.CookieMngrOptions{AllowHTTP: false})

	// Parse our template files
	tmpl = template.Must(template.New("templates").Delims("[[", "]]").ParseGlob("webui/templates/*.html"))

	// Connect to Minio server
	err = com.ConnectMinio()
	if err != nil {
		log.Fatalf(err.Error())
	}

	// Connect to PostgreSQL server
	err = com.ConnectPostgreSQL()
	if err != nil {
		log.Fatalf(err.Error())
	}

	// Connect to cache server
	err = com.ConnectCache()
	if err != nil {
		log.Fatalf(err.Error())
	}

	// Our pages
	http.HandleFunc("/", logReq(mainHandler))
	http.HandleFunc("/logout", logReq(logoutHandler))
	http.HandleFunc("/pref", logReq(prefHandler))
	http.HandleFunc("/register", logReq(createUserHandler))
	http.HandleFunc("/selectusername", logReq(selectUsernamePage))
	http.HandleFunc("/stars/", logReq(starsHandler))
	http.HandleFunc("/upload/", logReq(uploadFormHandler))
	http.HandleFunc("/vis/", logReq(visualisePage))
	http.HandleFunc("/x/callback", logReq(auth0CallbackHandler))
	http.HandleFunc("/x/download/", logReq(downloadHandler))
	http.HandleFunc("/x/downloadcsv/", logReq(downloadCSVHandler))
	http.HandleFunc("/x/star/", logReq(starToggleHandler))
	http.HandleFunc("/x/table/", logReq(tableViewHandler))
	http.HandleFunc("/x/uploaddata/", logReq(uploadDataHandler))
	http.HandleFunc("/x/visdata/", logReq(visData))

	// Static files
	http.HandleFunc("/images/auth0.svg", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join("webui", "images", "auth0.svg"))
	}))
	http.HandleFunc("/images/rackspace.svg", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join("webui", "images", "rackspace.svg"))
	}))
	http.HandleFunc("/images/sqlitebrowser.svg", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join("webui", "images", "sqlitebrowser.svg"))
	}))
	http.HandleFunc("/favicon.ico", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join("webui", "favicon.ico"))
	}))
	http.HandleFunc("/robots.txt", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join("webui", "robots.txt"))
	}))

	// Start server
	log.Printf("DBHub server starting on https://%s\n", com.WebServer())
	err = http.ListenAndServeTLS(com.WebServer(), com.WebServerCert(), com.WebServerCertKey(), nil)

	// Shut down nicely
	com.DisconnectPostgreSQL()

	if err != nil {
		log.Fatal(err)
	}
}

func mainHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Main handler"

	// Split the request URL into path components
	pathStrings := strings.Split(r.URL.Path, "/")

	// numPieces will be 2 if the request was for the root directory (https://server/), or if
	// the request included only a single path component (https://server/someuser/)
	numPieces := len(pathStrings)
	if numPieces == 2 {
		userName := pathStrings[1]
		// Check if the request was for the root directory
		if pathStrings[1] == "" {
			// Yep, root directory request
			frontPage(w, r)
			return
		}

		// The request was for a user page
		userPage(w, r, userName)
		return
	}

	userName := pathStrings[1]
	dbName := pathStrings[2]

	// Validate the user supplied user and database name
	err := com.ValidateUserDB(userName, dbName)
	if err != nil {
		log.Printf("%s: Validation failed of user or database name. Username: '%v', Database: '%s', Error: %s",
			pageName, userName, dbName, err)
		errorPage(w, r, http.StatusBadRequest, "Invalid user or database name")
		return
	}

	// This catches the case where a "/" is on the end of a user page URL
	// TODO: Refactor this and the above identical code.  Doing it this way is non-optimal
	if pathStrings[2] == "" {
		// The request was for a user page
		userPage(w, r, userName)
		return
	}

	// * A specific database was requested *

	// Check if a table name was also requested
	err = r.ParseForm()
	if err != nil {
		log.Printf("%s: Error with ParseForm() in main handler: %s\n", pageName, err)
	}
	dbTable := r.FormValue("table")

	// If a table name was supplied, validate it
	if dbTable != "" {
		err = com.ValidatePGTable(dbTable)
		if err != nil {
			// Validation failed, so don't pass on the table name
			log.Printf("%s: Validation failed for table name: %s", pageName, err)
			dbTable = ""
		}
	}

	// TODO: Add support for folders and sub-folders in request paths
	databasePage(w, r, userName, dbName, dbTable)
}

// This handles incoming requests for the preferences page by logged in users
func prefHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Preferences handler"

	// Ensure user is logged in
	var loggedInUser string
	validSession := false
	sess := session.Get(r)
	if sess != nil {
		u := sess.CAttr("UserName")
		if u != nil {
			loggedInUser = u.(string)
			validSession = true
		} else {
			session.Remove(sess, w)
		}
	}
	if validSession != true {
		// Display an error message
		// TODO: Show the login dialog
		errorPage(w, r, http.StatusInternalServerError, "Error: Must be logged in to view that page.")
		return
	}

	// Gather submitted form data (if any)
	err := r.ParseForm()
	if err != nil {
		log.Printf("%s: Error when parsing preference data: %s\n", pageName, err)
		errorPage(w, r, http.StatusBadRequest, "Error when parsing preference data")
		return
	}
	maxRows := r.PostFormValue("maxrows")

	// If no form data was submitted, display the preferences page form
	if maxRows == "" {
		prefPage(w, r, fmt.Sprintf("%s", loggedInUser))
		return
	}

	// Validate submitted form data
	err = com.Validate.Var(maxRows, "required,numeric,min=1,max=500")
	if err != nil {
		log.Printf("%s: Preference data failed validation: %s\n", pageName, err)
		errorPage(w, r, http.StatusBadRequest, "Error when parsing preference data")
		return
	}

	maxRowsNum, err := strconv.Atoi(maxRows)
	if err != nil {
		log.Printf("%s: Error converting string '%v' to integer: %s\n", pageName, maxRows, err)
		errorPage(w, r, http.StatusBadRequest, "Error when parsing preference data")
		return
	}

	// Update the preference data in the database
	err = com.SetPrefUserMaxRows(loggedInUser, maxRowsNum)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Error when updating preferences")
		return
	}

	// Bounce to the user home page
	http.Redirect(w, r, "/"+loggedInUser, http.StatusTemporaryRedirect)
}

// Handles JSON requests from the front end to toggle a database's star
func starToggleHandler(w http.ResponseWriter, r *http.Request) {
	// Extract the user and database name
	dbOwner, dbName, err := com.GetOD(2, r) // 2 = Ignore "/x/star/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Retrieve session data (if any)
	var loggedInUser string
	validSession := false
	sess := session.Get(r)
	if sess != nil {
		u := sess.CAttr("UserName")
		if u != nil {
			loggedInUser = u.(string)
			validSession = true
		} else {
			session.Remove(sess, w)
		}
	}

	// Ensure we have a valid logged in user
	if validSession != true {
		// No logged in username, so nothing to update
		fmt.Fprint(w, "-1") // -1 tells the front end not to update the displayed star count
		return
	}

	// Toggle on or off the starring of a database by a user
	err = com.ToggleDBStar(loggedInUser, dbOwner, dbName)
	if err != nil {
		fmt.Fprint(w, "-1") // -1 tells the front end not to update the displayed star count
		return
	}

	// Return the updated star count
	newStarCount, err := com.DBStars(dbOwner, dbName)
	if err != nil {
		fmt.Fprint(w, "-1") // -1 tells the front end not to update the displayed star count
		return
	}
	fmt.Fprint(w, newStarCount)
}

func starsHandler(w http.ResponseWriter, r *http.Request) {
	// Retrieve user and database name
	dbOwner, dbName, err := com.GetOD(1, r) // 2 = Ignore "/stars/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Render the stars page
	starsPage(w, r, dbOwner, dbName)
}

// This passes table row data back to the main UI in JSON format
func tableViewHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Table data handler"

	// Retrieve user, database, and table name
	dbOwner, dbName, requestedTable, dbVersion, err := com.GetODTV(2, r) // 1 = Ignore "/x/table/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		u := sess.CAttr("UserName")
		if u != nil {
			loggedInUser = u.(string)
		} else {
			session.Remove(sess, w)
		}
	}

	// Check if the user has access to the requested database
	bucket, id, err := com.MinioBucketID(dbOwner, dbName, dbVersion, loggedInUser)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Sanity check
	if id == "" {
		// The requested database wasn't found
		log.Printf("%s: Requested database not found. Owner: '%s' Database: '%s'", pageName, dbOwner, dbName)
		return
	}

	// Determine the number of rows to display
	var maxRows int
	if loggedInUser != "" {
		// Retrieve the user preference data
		maxRows = com.PrefUserMaxRows(loggedInUser)
	} else {
		// Not logged in, so default to 10 rows
		maxRows = 10
	}

	// Open the Minio database
	sdb, err := com.OpenMinioObject(bucket, id)

	// Retrieve the list of tables in the database
	tables, err := sdb.Tables("")
	if err != nil {
		log.Printf("Error retrieving table names: %s", err)
		return
	}
	if len(tables) == 0 {
		// No table names were returned, so abort
		log.Printf("The database '%s' doesn't seem to have any tables. Aborting.", dbName)
		return
	}

	// If a specific table was requested, check it exists
	if requestedTable != "" {
		tablePresent := false
		for _, tableName := range tables {
			if requestedTable == tableName {
				tablePresent = true
			}
		}
		if tablePresent == false {
			// The requested table doesn't exist
			errorPage(w, r, http.StatusBadRequest, "Requested table does not exist")
			return
		}
	}

	// If no specific table was requested, use the first one
	if requestedTable == "" {
		requestedTable = tables[0]
	}

	// Read the data from the database
	dataRows, err := com.ReadSQLiteDB(sdb, requestedTable, maxRows)
	if err != nil {
		// Some kind of error when reading the database data
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Count the total number of rows in the requested table
	dataRows.TotalRows, err = com.GetSQLiteRowCount(sdb, requestedTable)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Format the output
	var jsonResponse []byte
	if dataRows.RowCount > 0 {
		// Use json.MarshalIndent() for nicer looking output
		jsonResponse, err = json.MarshalIndent(dataRows, "", " ")
		if err != nil {
			log.Println(err)
			return
		}
	} else {
		// Return an empty set indicator, instead of "null"
		jsonResponse = []byte{'{', ']'}
	}

	//w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprintf(w, "%s", jsonResponse)
}

// This function presents the database upload form to logged in users
func uploadFormHandler(w http.ResponseWriter, r *http.Request) {
	// Retrieve session data (if any)
	var loggedInUser string
	validSession := false
	sess := session.Get(r)
	if sess != nil {
		u := sess.CAttr("UserName")
		if u != nil {
			loggedInUser = u.(string)
			validSession = true
		} else {
			session.Remove(sess, w)
		}
	}

	// Ensure we have a valid logged in user
	if validSession != true {
		errorPage(w, r, http.StatusUnauthorized, "You need to be logged in")
		return
	}

	// Render the upload page
	uploadPage(w, r, fmt.Sprintf("%s", loggedInUser))
}

// This function processes new database data submitted through the upload form
func uploadDataHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Upload DB handler"

	// Retrieve session data (if any)
	var loggedInUser string
	validSession := false
	sess := session.Get(r)
	if sess != nil {
		u := sess.CAttr("UserName")
		if u != nil {
			loggedInUser = u.(string)
			validSession = true
		} else {
			session.Remove(sess, w)
		}
	}

	// Ensure we have a valid logged in user
	if validSession != true {
		errorPage(w, r, http.StatusUnauthorized, "You need to be logged in")
		return
	}

	// Prepare the form data
	r.ParseMultipartForm(32 << 20) // 64MB of ram max
	if err := r.ParseForm(); err != nil {
		log.Printf("%s: ParseForm() error: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Grab and validate the supplied "public" form field
	userPublic := r.PostFormValue("public")
	public, err := strconv.ParseBool(userPublic)
	if err != nil {
		log.Printf("%s: Error when converting public value to boolean: %v\n", pageName, err)
		errorPage(w, r, http.StatusBadRequest, "Public value incorrect")
		return
	}

	// TODO: Add support for folders and subfolders
	folder := "/"

	tempFile, handler, err := r.FormFile("database")
	if err != nil {
		log.Printf("%s: Uploading file failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database file missing from upload data?")
		return
	}
	dbName := handler.Filename
	defer tempFile.Close()

	// Validate the database name
	err = com.ValidateDB(dbName)
	if err != nil {
		log.Printf("%s: Validation failed for database name: %s", pageName, err)
		errorPage(w, r, http.StatusBadRequest, "Invalid database name")
		return
	}

	// Write the temporary file locally, so we can try opening it with SQLite to verify it's ok
	var tempBuf bytes.Buffer
	bytesWritten, err := io.Copy(&tempBuf, tempFile)
	if err != nil {
		log.Printf("%s: Error: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}
	if bytesWritten == 0 {
		log.Printf("%s: Database seems to be 0 bytes in length. Username: %s, Database: %s\n", pageName,
			loggedInUser, dbName)
		errorPage(w, r, http.StatusBadRequest, "Database file is 0 length?")
		return
	}
	tempDB, err := ioutil.TempFile("", "dbhub-upload-")
	if err != nil {
		log.Printf("%s: Error creating temporary file. User: %s, Database: %s, Filename: %s, Error: %v\n",
			pageName, loggedInUser, dbName, tempDB.Name(), err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}
	_, err = tempDB.Write(tempBuf.Bytes())
	if err != nil {
		log.Printf("%s: Error when writing the uploaded db to a temp file. User: %s, Database: %s"+
			"Error: %v\n", pageName, loggedInUser, dbName, err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}
	tempDBName := tempDB.Name()

	// Delete the temporary file when this function finishes
	defer os.Remove(tempDBName)

	// Perform a read on the database, as a basic sanity check to ensure it's really a SQLite database
	sqliteDB, err := sqlite.Open(tempDBName, sqlite.OpenReadOnly)
	if err != nil {
		log.Printf("Couldn't open database when sanity checking upload: %s", err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}
	defer sqliteDB.Close()
	tables, err := sqliteDB.Tables("")
	if err != nil {
		log.Printf("Error retrieving table names when sanity checking upload: %s", err)
		errorPage(w, r, http.StatusInternalServerError,
			"Error when sanity checking file.  Possibly encrypted or not a database?")
		return
	}
	if len(tables) == 0 {
		// No table names were returned, so abort
		log.Printf("The attemped upload for '%s' failed, as it doesn't seem to have any tables.", dbName)
		errorPage(w, r, http.StatusInternalServerError, "Database has no tables?")
		return
	}

	// Generate sha256 of the uploaded file
	shaSum := sha256.Sum256(tempBuf.Bytes())

	// Determine the version number for this new database
	highVer, err := com.HighestDBVersion(loggedInUser, dbName, "/")
	var newVer int
	if highVer > 0 {
		// The database already exists
		newVer = highVer + 1
	} else {
		newVer = 1
	}

	// Retrieve the Minio bucket to store the database in
	bucket, err := com.MinioUserBucket(loggedInUser)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Database query failure")
		return
	}

	// Generate filename to store the database as
	var minioID string
	for okID := false; okID == false; {
		// Check if the randomly generated filename is available, just in caes
		minioID = com.RandomString(8) + ".db"
		okID, err = com.CheckMinioIDAvail(loggedInUser, minioID)
		if err != nil {
			errorPage(w, r, http.StatusInternalServerError, "Database query failure")
			return
		}
	}

	// Store the database file in Minio
	dbSize, err := com.StoreMinioObject(bucket, minioID, &tempBuf, handler.Header["Content-Type"][0])
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Storing database file failed")
		return
	}

	// Add the database file details to PostgreSQL
	err = com.AddDatabase(loggedInUser, folder, dbName, newVer, shaSum[:], dbSize, public, bucket, minioID)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Adding database details to PostgreSQL failed")
		return
	}

	// Log the successful database upload
	log.Printf("%s: Username: %v, database '%v' uploaded as '%v', bytes: %v\n", pageName, loggedInUser, dbName,
		minioID, dbSize)

	// Database upload succeeded.  Tell the user then bounce back to their profile page
	fmt.Fprintf(w, `
	<html><head><script type="text/javascript"><!--
		function delayer(){
			window.location = "/%s"
		}//-->
	</script></head>
	<body onLoad="setTimeout('delayer()', 5000)">
	<body>Upload succeeded<br /><br /><a href="/%s">Continuing to profile page...</a></body></html>`,
		loggedInUser, loggedInUser)
}

// Receives a request for specific table data from the front end, returning it as JSON
func visData(w http.ResponseWriter, r *http.Request) {
	pageName := "Visualisation data handler"

	var pageData struct {
		Meta com.MetaInfo
		DB   com.SQLiteDBinfo
		Data com.SQLiteRecordSet
	}

	// Retrieve user, database, and table name
	userName, dbName, requestedTable, err := com.GetODT(2, r) // 1 = Ignore "/x/table/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Check if X and Y column names were given
	var reqXCol, reqYCol, xCol, yCol string
	reqXCol = r.FormValue("xcol")
	reqYCol = r.FormValue("ycol")

	// Validate column names if present
	// FIXME: Create a proper validation function for SQLite column names
	if reqXCol != "" {
		err = com.ValidatePGTable(reqXCol)
		if err != nil {
			log.Printf("Validation failed for SQLite column name: %s", err)
			return
		}
		xCol = reqXCol
	}
	if reqYCol != "" {
		err = com.ValidatePGTable(reqYCol)
		if err != nil {
			log.Printf("Validation failed for SQLite column name: %s", err)
			return
		}
		yCol = reqYCol
	}

	// Validate WHERE clause values if present
	var reqWCol, reqWType, reqWVal, wCol, wType, wVal string
	reqWCol = r.FormValue("wherecol")
	reqWType = r.FormValue("wheretype")
	reqWVal = r.FormValue("whereval")

	// WHERE column
	if reqWCol != "" {
		err = com.ValidatePGTable(reqWCol)
		if err != nil {
			log.Printf("Validation failed for SQLite column name: %s", err)
			return
		}
		wCol = reqWCol
	}

	// WHERE type
	switch reqWType {
	case "":
		// We don't pass along empty values
	case "LIKE", "=", "!=", "<", "<=", ">", ">=":
		wType = reqWType
	default:
		// This should never be reached
		log.Printf("%s: Validation failed on WHERE clause type. wType = '%v'\n", pageName, wType)
		return
	}

	// TODO: Add ORDER BY clause
	// TODO: We'll probably need some kind of optional data transformation for columns too
	// TODO    eg column foo → DATE (type)

	// WHERE value
	var whereClauses []com.WhereClause
	if reqWVal != "" && wType != "" {
		whereClauses = append(whereClauses, com.WhereClause{Column: wCol, Type: wType, Value: reqWVal})

		// TODO: Double check if we should be filtering out potentially devious characters here. I don't
		// TODO  (at the moment) *think* we need to, as we're using parameter binding on the passed in values
		wVal = reqWVal
	}

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		u := sess.CAttr("UserName")
		if u != nil {
			loggedInUser = u.(string)
		} else {
			session.Remove(sess, w)
		}
	}

	// Check if the user has access to the requested database
	err = com.CheckUserDBAccess(&pageData.DB, loggedInUser, userName, dbName)
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// * Execution can only get here if the user has access to the requested database *

	// Generate a predictable cache key for the JSON data
	var pageCacheKey string
	if loggedInUser != userName {
		tempArr := md5.Sum([]byte(userName + "/" + dbName + "/" + requestedTable + xCol + yCol + wCol +
			wType + wVal))
		pageCacheKey = "visdat-pub-" + hex.EncodeToString(tempArr[:])
	} else {
		tempArr := md5.Sum([]byte(loggedInUser + "-" + userName + "/" + dbName + "/" + requestedTable +
			xCol + yCol + wCol + wType + wVal))
		pageCacheKey = "visdat-" + hex.EncodeToString(tempArr[:])
	}

	// If a cached version of the page data exists, use it
	var jsonResponse []byte
	ok, err := com.GetCachedData(pageCacheKey, &jsonResponse)
	if err != nil {
		log.Printf("%s: Error retrieving page data from cache: %v\n", pageName, err)
	}
	if ok {
		// Render the JSON response from cache
		fmt.Fprintf(w, "%s", jsonResponse)
		return
	}

	// Get a handle from Minio for the database object
	sdb, err := com.OpenMinioObject(pageData.DB.MinioBkt, pageData.DB.MinioId)
	if err != nil {
		return
	}

	// Retrieve the list of tables in the database
	tables, err := sdb.Tables("")
	if err != nil {
		log.Printf("%s: Error retrieving table names: %s", pageName, err)
		return
	}
	if len(tables) == 0 {
		// No table names were returned, so abort
		log.Printf("%s: The database '%s' doesn't seem to have any tables. Aborting.", pageName, dbName)
		return
	}
	pageData.DB.Info.Tables = tables

	// If a specific table was requested, check that it's present
	var dbTable string
	if requestedTable != "" {
		// Check the requested table is present
		for _, tbl := range tables {
			if tbl == requestedTable {
				dbTable = requestedTable
			}
		}
	}

	// If a specific table wasn't requested, use the first table in the database
	if dbTable == "" {
		dbTable = pageData.DB.Info.Tables[0]
	}

	// Retrieve the table data requested by the user
	maxVals := 2500 // 2500 row maximum for now
	if xCol != "" && yCol != "" {
		pageData.Data, err = com.ReadSQLiteDBCols(sdb, requestedTable, true, true, maxVals, whereClauses, xCol, yCol)
	} else {
		pageData.Data, err = com.ReadSQLiteDB(sdb, requestedTable, maxVals)
	}
	if err != nil {
		// Some kind of error when reading the database data
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Use json.MarshalIndent() for nicer looking output
	jsonResponse, err = json.Marshal(pageData.Data)
	if err != nil {
		log.Println(err)
		return
	}

	// Cache the JSON data
	err = com.CacheData(pageCacheKey, jsonResponse, com.CacheTime)
	if err != nil {
		log.Printf("%s: Error when caching JSON data: %v\n", pageName, err)
	}

	//w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprintf(w, "%s", jsonResponse)
}
