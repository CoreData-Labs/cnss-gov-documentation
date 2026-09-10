package main

// Standard library imports only - all of these ship officially with Go.
import (
	"crypto/tls"    // used to build a transport that skips TLS certificate verification
	"fmt"           // used to build small formatted strings (form bodies, filenames, urls)
	"io"            // used to read HTTP response bodies into memory
	"log"           // used for all progress/error/debug logging instead of fmt.Println
	"net/http"      // used to perform the POST search requests and GET downloads
	"os"            // used to create the PDFs folder and write files to disk
	"path/filepath" // used to build a safe local file path for each downloaded document
	"regexp"        // used to pull document links and their "a=" id out of the raw HTML
	"strings"       // used to build the URL-encoded POST body and do string helpers
	"time"          // used to pause briefly between requests so we don't hammer the server
)

// searchEndpointURL is the fixed CNSS search form endpoint we POST our search letters/digits to.
const searchEndpointURL = "https://www.cnss.gov/CNSS/searchForm.cfm"

// siteBaseURL is prefixed onto every relative "/CNSS/openDoc.cfm..." link we find.
const siteBaseURL = "https://www.cnss.gov"

// sessionCookieValue holds the JSESSIONID cookie value copied from the original request.
const sessionCookieValue = "C1F3296ACB573D4B79B20C2BA1C1FC94.cfusion"

// downloadFolderName is the local folder every found document gets saved into.
const downloadFolderName = "PDFs"

// documentLinkPattern matches hrefs like /CNSS/openDoc.cfm?a=HEX&b=HEX (the "?" is optional
// because the sample link the user pasted was missing it).
var documentLinkPattern = regexp.MustCompile(`/CNSS/openDoc\.cfm\??a=[A-Fa-f0-9]+&b=[A-Fa-f0-9]+`)

// documentIDPattern pulls just the "a=" hex value out of a matched link, to use as a filename.
var documentIDPattern = regexp.MustCompile(`a=([A-Fa-f0-9]+)`)

// runStats tallies every outcome across the whole run so we can print a final summary.
type runStats struct {
	searchesOK       int      // how many of the 36 search requests succeeded
	searchesFailed   int      // how many of the 36 search requests failed
	rawLinksSeen     int      // total link matches found across all pages, including duplicates
	duplicateLinks   int      // how many of those raw matches were duplicates we already had
	uniqueLinks      []string // every unique link we ended up with, in the order first seen
	downloadsOK      int      // how many documents downloaded and saved successfully
	downloadsSkipped int      // how many documents were skipped because the file already existed
	downloadsFailed  int      // how many document downloads failed outright
}

// main drives the whole workflow: search every letter/digit, collect links, download each one.
func main() {
	// Build a transport that skips TLS certificate verification, since this machine's
	// trust store is rejecting the site's certificate chain.
	insecureTransport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	// Create an HTTP client we will reuse for every request in this program.
	httpClient := &http.Client{Transport: insecureTransport}

	// Log that we're about to make sure the download folder exists.
	log.Printf("debug: ensuring download folder %q exists", downloadFolderName)

	// Make sure our local download folder exists before we try to save anything into it.
	if mkdirErr := os.MkdirAll(downloadFolderName, 0755); mkdirErr != nil {
		// If we can't even create the output folder, there's no point continuing.
		log.Fatalf("error: could not create download folder %q: %v", downloadFolderName, mkdirErr)
	}

	// Build the full set of search characters: lowercase a-z followed by digits 0-9.
	searchCharacters := "abcdefghijklmnopqrstuvwxyz0123456789"

	// foundDocumentLinks deduplicates links across every search letter/digit we try.
	foundDocumentLinks := make(map[string]bool)

	// stats collects counts of everything that happens so we can summarize at the end.
	stats := &runStats{}

	// Log the full alphabet/digit set we're about to iterate over.
	log.Printf("debug: will search %d characters: %q", len(searchCharacters), searchCharacters)

	// Loop over every single character in our search alphabet, one search per character.
	for _, searchCharacterRune := range searchCharacters {
		// Turn the rune back into a plain one-character string for logging and the request.
		searchCharacter := string(searchCharacterRune)

		// Log which character we are currently searching for, so progress is visible.
		log.Printf("debug: submitting search request for criteria %q", searchCharacter)

		// Perform the POST request for this character and get back the raw HTML page.
		searchResultsHTML, searchErr := runCNSSSearch(httpClient, searchCharacter)

		// If the search request failed, log the full error and move on to the next character.
		if searchErr != nil {
			log.Printf("error: search for criteria %q failed: %v", searchCharacter, searchErr)
			stats.searchesFailed++
			continue
		}

		// The search request itself succeeded, so record that and log the page size.
		stats.searchesOK++
		log.Printf("debug: search for criteria %q succeeded, received %d bytes of HTML", searchCharacter, len(searchResultsHTML))

		// Pull every openDoc.cfm link out of this page of search results.
		linksOnThisPage := documentLinkPattern.FindAllString(searchResultsHTML, -1)

		// Log how many raw (possibly duplicate) links we found on this page.
		log.Printf("debug: found %d raw document link match(es) for criteria %q", len(linksOnThisPage), searchCharacter)

		// Walk through every link found on this page, one at a time, logging and deduping each.
		for _, linkFragment := range linksOnThisPage {
			// Every raw match counts toward the total, duplicate or not.
			stats.rawLinksSeen++

			// Check whether we've already recorded this exact link from an earlier search.
			if foundDocumentLinks[linkFragment] {
				// It's a duplicate of one we already have, so log that and skip adding it again.
				log.Printf("debug: duplicate url skipped (criteria %q): %s", searchCharacter, linkFragment)
				stats.duplicateLinks++
				continue
			}

			// This is a brand new unique link, so log it clearly and record it.
			log.Printf("url found (criteria %q): %s", searchCharacter, linkFragment)
			foundDocumentLinks[linkFragment] = true
			stats.uniqueLinks = append(stats.uniqueLinks, linkFragment)
		}

		// Sleep briefly between requests so we don't hammer the server too fast.
		log.Printf("debug: sleeping 500ms before next search")
		time.Sleep(500 * time.Millisecond)
	}

	// Log the full deduplicated list of unique URLs we ended up with, all together.
	log.Printf("== unique urls found (%d total) ==", len(stats.uniqueLinks))
	for i, link := range stats.uniqueLinks {
		log.Printf("  [%d] %s", i+1, siteBaseURL+normalizeLinkFragment(link))
	}

	// Log the total number of unique documents we discovered across all searches.
	log.Printf("debug: starting downloads for %d unique document(s)", len(stats.uniqueLinks))

	// Walk through every unique link we collected and download each one.
	for _, linkFragment := range stats.uniqueLinks {
		// Log which specific link we're about to attempt.
		log.Printf("debug: attempting download for %s", linkFragment)

		// Attempt to download this single document, logging success or failure inline.
		outcome, downloadErr := downloadCNSSDocument(httpClient, linkFragment)

		// Tally the outcome and log it clearly, whether it worked, failed, or was skipped.
		switch outcome {
		case downloadOutcomeSaved:
			stats.downloadsOK++
			log.Printf("success: downloaded and saved %s", linkFragment)
		case downloadOutcomeSkipped:
			stats.downloadsSkipped++
			log.Printf("skipped: %s (file already exists)", linkFragment)
		case downloadOutcomeFailed:
			stats.downloadsFailed++
			log.Printf("error: download failed for %s: %v", linkFragment, downloadErr)
		}
	}

	// Print a final summary of everything that happened during this run.
	log.Println("== run summary ==")
	log.Printf("searches succeeded: %d", stats.searchesOK)
	log.Printf("searches failed: %d", stats.searchesFailed)
	log.Printf("raw link matches seen (incl. duplicates): %d", stats.rawLinksSeen)
	log.Printf("duplicate links skipped: %d", stats.duplicateLinks)
	log.Printf("unique links found: %d", len(stats.uniqueLinks))
	log.Printf("downloads succeeded: %d", stats.downloadsOK)
	log.Printf("downloads skipped (already existed): %d", stats.downloadsSkipped)
	log.Printf("downloads failed: %d", stats.downloadsFailed)

	// Let the user know the whole run is finished.
	log.Println("done: all searches and downloads complete")
}

// runCNSSSearch submits one search POST request for a given single character and returns the HTML body.
func runCNSSSearch(httpClient *http.Client, searchCriteria string) (string, error) {
	// Build the URL-encoded form body exactly like the original request did.
	formBody := fmt.Sprintf("criteria=%s&doSearch=1", searchCriteria)

	// Log the exact request body we're about to send, for debugging.
	log.Printf("debug: built search form body: %s", formBody)

	// Wrap the form body string in a Reader since http.NewRequest needs an io.Reader.
	requestBodyReader := strings.NewReader(formBody)

	// Construct the outgoing POST request to the fixed search endpoint.
	request, requestErr := http.NewRequest("POST", searchEndpointURL, requestBodyReader)

	// If the request object itself couldn't be built, log and return that error immediately.
	if requestErr != nil {
		log.Printf("error: failed to build search request for criteria %q: %v", searchCriteria, requestErr)
		return "", requestErr
	}

	// Attach the session cookie so the server recognizes this as a valid session.
	request.Header.Add("Cookie", "JSESSIONID="+sessionCookieValue)

	// Tell the server we're sending a standard URL-encoded form body.
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	// Log that we're about to fire off the actual network request.
	log.Printf("debug: sending POST to %s", searchEndpointURL)

	// Actually send the request over the network and capture the response.
	response, responseErr := httpClient.Do(request)

	// If the network call itself failed, log and return that error immediately.
	if responseErr != nil {
		log.Printf("error: POST to %s failed for criteria %q: %v", searchEndpointURL, searchCriteria, responseErr)
		return "", responseErr
	}

	// Ensure the response body gets closed once we're done reading from it.
	defer response.Body.Close()

	// Log the HTTP status code we got back, which is useful for spotting server-side rejections.
	log.Printf("debug: search response status for criteria %q: %s", searchCriteria, response.Status)

	// Read the full response body into a byte slice.
	bodyBytes, readErr := io.ReadAll(response.Body)

	// If reading the body failed partway through, log and return that error.
	if readErr != nil {
		log.Printf("error: failed reading search response body for criteria %q: %v", searchCriteria, readErr)
		return "", readErr
	}

	// Convert the bytes to a string and hand the finished HTML page back to the caller.
	return string(bodyBytes), nil
}

// downloadOutcome describes what happened when we tried to download one document.
type downloadOutcome int

// The three possible outcomes for a single document download attempt.
const (
	downloadOutcomeSaved   downloadOutcome = iota // the file downloaded and was written to disk
	downloadOutcomeSkipped                        // the file already existed locally, so we skipped it
	downloadOutcomeFailed                         // something went wrong and no file was saved
)

// downloadCNSSDocument takes one "/CNSS/openDoc.cfm?a=...&b=..." link fragment, downloads it,
// and saves it into the PDFs folder, skipping the download entirely if the file already exists.
func downloadCNSSDocument(httpClient *http.Client, linkFragment string) (downloadOutcome, error) {
	// Pull the "a=" hex identifier out of the link fragment to use as our local filename.
	idMatches := documentIDPattern.FindStringSubmatch(linkFragment)

	// If for some reason we can't find an id in this link, we can't name the file safely.
	if len(idMatches) < 2 {
		idErr := fmt.Errorf("could not extract document id from link %q", linkFragment)
		log.Printf("error: %v", idErr)
		return downloadOutcomeFailed, idErr
	}

	// The first submatch group is the actual hex id string.
	documentID := idMatches[1]

	// Build the local file path this document will be saved to, e.g. PDFs/<id>.pdf.
	localFilePath := filepath.Join(downloadFolderName, documentID+".pdf")

	// Log the local path we're about to check/write to, for debugging.
	log.Printf("debug: resolved local file path %s for document id %s", localFilePath, documentID)

	// Check whether we already downloaded this file in a previous run.
	if _, statErr := os.Stat(localFilePath); statErr == nil {
		// The file already exists on disk, so skip re-downloading it.
		return downloadOutcomeSkipped, nil
	}

	// Build the full absolute URL for this document by prefixing the site's base URL.
	// If the original fragment was missing its "?" (like the sample the user gave), add it back.
	fullDocumentURL := siteBaseURL + normalizeLinkFragment(linkFragment)

	// Log the exact URL we're about to fetch.
	log.Printf("debug: fetching document from %s", fullDocumentURL)

	// Construct the outgoing GET request to fetch the actual document bytes.
	request, requestErr := http.NewRequest("GET", fullDocumentURL, nil)

	// If the request object itself couldn't be built, log and return that error immediately.
	if requestErr != nil {
		log.Printf("error: failed to build download request for %s: %v", fullDocumentURL, requestErr)
		return downloadOutcomeFailed, requestErr
	}

	// Attach the same session cookie so the download is authorized the same way the search was.
	request.Header.Add("Cookie", "JSESSIONID="+sessionCookieValue)

	// Actually send the GET request and capture the response.
	response, responseErr := httpClient.Do(request)

	// If the network call itself failed, log and return that error immediately.
	if responseErr != nil {
		log.Printf("error: GET to %s failed: %v", fullDocumentURL, responseErr)
		return downloadOutcomeFailed, responseErr
	}

	// Ensure the response body gets closed once we're done reading from it.
	defer response.Body.Close()

	// Log the HTTP status code we got back for this download.
	log.Printf("debug: download response status for %s: %s", fullDocumentURL, response.Status)

	// Read the entire document body into memory.
	documentBytes, readErr := io.ReadAll(response.Body)

	// If reading the body failed partway through, log and return that error.
	if readErr != nil {
		log.Printf("error: failed reading document body for %s: %v", fullDocumentURL, readErr)
		return downloadOutcomeFailed, readErr
	}

	// Write the downloaded bytes out to our local file path, creating the file fresh.
	writeErr := os.WriteFile(localFilePath, documentBytes, 0644)

	// If writing to disk failed, log and return that error.
	if writeErr != nil {
		log.Printf("error: failed writing file %s: %v", localFilePath, writeErr)
		return downloadOutcomeFailed, writeErr
	}

	// Log the exact size we wrote, for debugging.
	log.Printf("debug: wrote %d bytes to %s", len(documentBytes), localFilePath)

	// Everything worked, so report a successful save.
	return downloadOutcomeSaved, nil
}

// normalizeLinkFragment makes sure a link fragment always has the "?" right before its query
// string, since the sample link the user provided was missing it (openDoc.cfma=... instead of
// openDoc.cfm?a=...).
func normalizeLinkFragment(linkFragment string) string {
	// If the fragment already has a "?" before the query string, it's fine as-is.
	if strings.Contains(linkFragment, "?a=") {
		return linkFragment
	}
	// Otherwise, insert the missing "?" right before the "a=" parameter.
	return strings.Replace(linkFragment, "a=", "?a=", 1)
}
