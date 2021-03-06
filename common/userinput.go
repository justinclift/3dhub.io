// These functions extract (and validate) user provided form data.
package common

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Extracts a database name from GET or POST/PUT data.
func GetDatabase(r *http.Request, allowGet bool) (string, error) {
	// Retrieve the variable from the GET or POST/PUT data
	var d string
	if allowGet {
		d = r.FormValue("dbname")
	} else {
		d = r.PostFormValue("dbname")
	}

	// Unescape, then validate the database name
	fileName, err := url.QueryUnescape(d)
	if err != nil {
		return "", err
	}
	err = ValidateFileName(fileName)
	if err != nil {
		log.Printf("Validation failed for database name '%s': %s", fileName, err)
		return "", errors.New("Invalid database name")
	}
	return fileName, nil
}

// Returns the folder name (if any) present in GET or POST/PUT data.
func GetFolder(r *http.Request, allowGet bool) (string, error) {
	// Retrieve the variable from the GET or POST/PUT data
	var f string
	if allowGet {
		f = r.FormValue("folder")
	} else {
		f = r.PostFormValue("folder")
	}

	// If no folder given, return
	if f == "" {
		return "", nil
	}

	// Unescape, then validate the folder name
	folder, err := url.QueryUnescape(f)
	if err != nil {
		return "", err
	}
	err = ValidateFolder(folder)
	if err != nil {
		log.Printf("Validation failed for folder: '%s': %s", folder, err)
		return "", err
	}

	return folder, nil
}

// Return the requested branch name, from get or post data.
func GetFormBranch(r *http.Request) (string, error) {
	// If no branch was given in the input, returns an empty string
	a := r.FormValue("branch")
	if a == "" {
		return "", nil
	}

	// Unescape, then validate the branch name
	b, err := url.QueryUnescape(a)
	if err != nil {
		return "", err
	}
	err = ValidateBranchName(b)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Invalid branch name: '%v'", b))
	}
	return b, nil
}

// Return the requested database commit, from form data.
func GetFormCommit(r *http.Request) (string, error) {
	// If no commit was given in the input, returns an empty string
	c := r.FormValue("commit")
	if c == "" {
		return "", nil
	}
	err := ValidateCommitID(c)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Invalid database commit: '%v'", c))
	}
	return c, nil
}

// Returns the licence name (if any) present in the form data
func GetFormLicence(r *http.Request) (licenceName string, err error) {
	// If no licence name given, return an empty string
	l := r.PostFormValue("licence")
	if l == "" {
		return "", nil
	}

	// Validate the licence name
	err = ValidateLicence(l)
	if err != nil {
		log.Printf("Validation failed for licence: '%s': %s", l, err)
		return "", err
	}
	licenceName = l

	return licenceName, nil
}

// Returns the source URL (if any) present in the form data
func GetFormSourceURL(r *http.Request) (sourceURL string, err error) {
	// Validate the source URL
	su := r.PostFormValue("sourceurl")
	if su != "" {
		err = Validate.Var(su, "url,min=5,max=255") // 255 seems like a reasonable first guess
		if err != nil {
			return sourceURL, errors.New("Validation failed for source URL field")
		}
		sourceURL = su
	}

	return sourceURL, err
}

// Return the requested release name, from get or post data.
func GetFormRelease(r *http.Request) (release string, err error) {
	// If no release was given in the input, returns an empty string
	a := r.FormValue("release")
	if a == "" {
		return "", nil
	}

	// Unescape, then validate the release name
	c, err := url.QueryUnescape(a)
	if err != nil {
		return "", err
	}
	err = ValidateBranchName(c)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Invalid release name: '%v'", c))
	}
	return c, nil
}

// Return the requested tag name, from get or post data.
func GetFormTag(r *http.Request) (tag string, err error) {
	// If no tag was given in the input, returns an empty string
	a := r.FormValue("tag")
	if a == "" {
		return "", nil
	}

	// Unescape, then validate the tag name
	c, err := url.QueryUnescape(a)
	if err != nil {
		return "", err
	}
	err = ValidateBranchName(c)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Invalid tag name: '%v'", c))
	}
	return c, nil
}

// Return the username, database, and commit (if any) present in the form data.
func GetFormUDC(r *http.Request) (string, string, string, error) {
	// Extract the username
	userName, err := GetUsername(r, false)
	if err != nil {
		return "", "", "", err
	}

	// Extract the database name
	fileName, err := GetDatabase(r, false)
	if err != nil {
		return "", "", "", err
	}

	// Extract the commit string
	commitID, err := GetFormCommit(r)
	if err != nil {
		return "", "", "", err
	}

	return userName, fileName, commitID, nil
}

// Returns the requested database owner and database name.
func GetOD(ignore_leading int, r *http.Request) (string, string, error) {
	// Split the request URL into path components
	pathStrings := strings.Split(r.URL.Path, "/")

	// Check that at least an owner/database combination was requested
	if len(pathStrings) < (3 + ignore_leading) {
		log.Printf("Something wrong with the requested URL: %v\n", r.URL.Path)
		return "", "", errors.New("Invalid URL")
	}
	owner := pathStrings[1+ignore_leading]
	fileName := pathStrings[2+ignore_leading]

	// Validate the user supplied owner and database name
	err := ValidateUserFilename(owner, fileName)
	if err != nil {
		// Don't bother logging the fairly common case of a bot using an AngularJS phrase in a request
		if (owner == "{{ meta.Owner + '" && fileName == "' + row.Database }}") ||
			(owner == "{{ row.owner + '" && fileName == "' + row.dbname }}") {
			return "", "", errors.New("Invalid owner or database name")
		}

		log.Printf("Validation failed for owner or database name. Owner '%s', DB name '%s': %s",
			owner, fileName, err)
		return "", "", errors.New("Invalid owner or database name")
	}

	// Everything seems ok
	return owner, fileName, nil
}

// Returns the requested database owner, database name, and commit revision.
func GetODC(ignore_leading int, r *http.Request) (string, string, string, error) {
	// Grab owner and database name
	owner, fileName, err := GetOD(ignore_leading, r)
	if err != nil {
		return "", "", "", err
	}

	// Extract the commit revision
	commitID, err := GetFormCommit(r)
	if err != nil {
		return "", "", "", err
	}

	// Everything seems ok
	return owner, fileName, commitID, nil
}

// Returns the requested database owner, database name, and table name.
func GetODT(ignore_leading int, r *http.Request) (string, string, string, error) {
	// Grab owner and database name
	owner, fileName, err := GetOD(ignore_leading, r)
	if err != nil {
		return "", "", "", err
	}

	// If a specific table was requested, get that info too
	requestedTable, err := GetTable(r)
	if err != nil {
		return "", "", "", err
	}

	// Everything seems ok
	return owner, fileName, requestedTable, nil
}

// Returns the requested database owner, database name, table name, and commit string.
func GetODTC(ignore_leading int, r *http.Request) (string, string, string, string, error) {
	// Grab owner and database name
	owner, fileName, err := GetOD(ignore_leading, r)
	if err != nil {
		return "", "", "", "", err
	}

	// If a specific table was requested, get that info too
	requestedTable, err := GetTable(r)
	if err != nil {
		return "", "", "", "", err
	}

	// Extract the commit string
	commitID, err := GetFormCommit(r)
	if err != nil {
		return "", "", "", "", err
	}

	// Everything seems ok
	return owner, fileName, requestedTable, commitID, nil
}

// Returns the requested "public" variable, if present in the form data.
// If something goes wrong, it defaults to "false".
func GetPub(r *http.Request) (bool, error) {
	val := r.PostFormValue("public")
	if val == "" {
		// No public/private variable found
		return false, errors.New("No public/private value present")
	}
	pub, err := strconv.ParseBool(val)
	if err != nil {
		log.Printf("Error when converting public value to boolean: %v\n", err)
		return false, err
	}

	return pub, nil
}

// Returns the requested table name (if any).
func GetTable(r *http.Request) (string, error) {
	var requestedTable string
	requestedTable = r.FormValue("table")

	// If a table name was supplied, validate it
	// FIXME: We should probably create a validation function for SQLite table names, not use our one for PG
	if requestedTable != "" {
		err := ValidatePGTable(requestedTable)
		if err != nil {
			// If the failed table name is "{{ db.Tablename }}", don't bother logging it.  It's just a
			// search bot picking up the AngularJS string then doing a request with it
			if requestedTable != "{{ db.Tablename }}" {
				log.Printf("Validation failed for table name: '%s': %s", requestedTable, err)
			}
			return "", errors.New("Invalid table name")
		}
	}

	// Everything seems ok
	return requestedTable, nil
}

// Return the username, folder, and database name (if any) present in the form data.
func GetUFD(r *http.Request, allowGet bool) (string, string, string, error) {
	// Extract the username
	userName, err := GetUsername(r, allowGet)
	if err != nil {
		return "", "", "", err
	}

	// Extract the folder
	folder, err := GetFolder(r, allowGet)
	if err != nil {
		return "", "", "", err
	}

	// Extract the database name
	fileName, err := GetDatabase(r, allowGet)
	if err != nil {
		return "", "", "", err
	}

	return userName, folder, fileName, nil
}

// Return the username (if any) present in the GET or POST/PUT data.
func GetUsername(r *http.Request, allowGet bool) (string, error) {
	// Retrieve the variable from the GET or POST/PUT data
	var u string
	if allowGet {
		u = r.FormValue("username")
	} else {
		u = r.PostFormValue("username")
	}

	// If no username given, return
	if u == "" {
		return "", nil
	}

	// Unescape, then validate the user name
	userName, err := url.QueryUnescape(u)
	if err != nil {
		return "", err
	}
	err = ValidateUser(userName)
	if err != nil {
		log.Printf("Validation failed for username: %s", err)
		return "", err
	}

	return userName, nil
}
