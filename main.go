package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dropbox/dropbox-sdk-go-unofficial/v6/dropbox"
	"github.com/dropbox/dropbox-sdk-go-unofficial/v6/dropbox/files"
	"github.com/dropbox/dropbox-sdk-go-unofficial/v6/dropbox/sharing"
)

// RSS is the root element of the RSS feed
type RSS struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	XMLNS   string   `xml:"xmlns:itunes,attr"`
	Channel Channel  `xml:"channel"`
}

// Channel contains information about the podcast channel
type Channel struct {
	Title        string      `xml:"title"`
	Link         string      `xml:"link"`
	Description  string      `xml:"description"`
	ItunesAuthor string      `xml:"itunes:author"`
	ItunesImage  ItunesImage `xml:"itunes:image"`
	Items        []Item      `xml:"item"`
}

// ItunesImage represents the podcast's cover image
type ItunesImage struct {
	Href string `xml:"href,attr"`
}

// Item represents a single episode of the podcast
type Item struct {
	Title     string    `xml:"title"`
	Link      string    `xml:"link"`
	GUID      string    `xml:"guid"`
	PubDate   string    `xml:"pubDate"`
	Enclosure Enclosure `xml:"enclosure"`
	//ItunesDuration string    `xml:"itunes:duration"`
}

// Enclosure represents the media file associated with an item
type Enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

// FFProbeOutput is used to unmarshal the JSON output of ffprobe
type FFProbeOutput struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

const (
	maxRetries = 3
	retryDelay = 2 * time.Second
)

func main() {
	//if _, err := exec.LookPath("ffprobe"); err != nil {
	//	log.Fatal("ffprobe not found in PATH. Please install ffmpeg.")
	//}

	if len(os.Args) < 4 { // dropboxPath, baseURL, imageURL
		fmt.Println("Usage: go run main.go <dropboxPath> <baseURL> <imageURL>")
		fmt.Println("Refresh token must be set via environment variable DROPBOX_REFRESH_TOKEN or will prompt with interactive flow if unset.")
		os.Exit(1)
	}
	dropboxPath := os.Args[1]
	baseURL := os.Args[2]
	imageURL := os.Args[3]

	refreshToken := os.Getenv("DROPBOX_REFRESH_TOKEN")
	if refreshToken == "" || refreshToken == "-" {
		var err error
		refreshToken, err = interactiveAuthFlow()
		if err != nil {
			log.Fatalf("OAuth flow failed: %v", err)
		}
		log.Printf("Obtained refresh token. Store it securely and set DROPBOX_REFRESH_TOKEN to avoid interactive prompts.")
	}

	// Remove trailing slash from dropbox path
	dropboxPath = strings.TrimSuffix(dropboxPath, "/")

	// Exchange refresh token for short-lived access token
	accessToken, err := fetchAccessToken(refreshToken)
	if err != nil {
		log.Fatalf("Failed to obtain access token: %v", err)
	}

	config := dropbox.Config{Token: accessToken}
	dbxf := files.New(config)
	dbxs := sharing.New(config)

	audioFiles, err := listAudioFiles(dbxf, dropboxPath)
	if err != nil {
		log.Fatalf("Failed to list files in Dropbox: %v", err)
	}

	items, err := buildItems(dbxs, audioFiles)
	if err != nil {
		log.Fatalf("Failed to build feed items: %v", err)
	}

	rss := RSS{
		Version: "2.0",
		XMLNS:   "http://www.itunes.com/dtds/podcast-1.0.dtd",
		Channel: Channel{
			Title:        "Reilly's Awesome Podcast",
			Link:         baseURL,
			Description:  "It's Reilly's Podcast, Baby!",
			ItunesAuthor: "Reilly Watson",
			ItunesImage: ItunesImage{
				Href: imageURL,
			},
			Items: items,
		},
	}

	xmlData, err := xml.MarshalIndent(rss, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal XML: %s", err)
	}

	fmt.Println(string(xmlData))
}

func listAudioFiles(dbxf files.Client, dropboxPath string) ([]*files.FileMetadata, error) {
	listFolderArg := files.NewListFolderArg(dropboxPath)
	listFolderResult, err := withRetry("list Dropbox folder", func() (*files.ListFolderResult, error) {
		return dbxf.ListFolder(listFolderArg)
	})
	if err != nil {
		return nil, err
	}

	var audioFiles []*files.FileMetadata
	appendAudioFiles(&audioFiles, listFolderResult.Entries)

	for listFolderResult.HasMore {
		continueArg := files.NewListFolderContinueArg(listFolderResult.Cursor)
		listFolderResult, err = withRetry("continue Dropbox folder listing", func() (*files.ListFolderResult, error) {
			return dbxf.ListFolderContinue(continueArg)
		})
		if err != nil {
			return nil, err
		}
		appendAudioFiles(&audioFiles, listFolderResult.Entries)
	}

	sort.Slice(audioFiles, func(i, j int) bool {
		if audioFiles[i].ServerModified.Equal(audioFiles[j].ServerModified) {
			leftName := strings.ToLower(audioFiles[i].Name)
			rightName := strings.ToLower(audioFiles[j].Name)
			if leftName == rightName {
				return audioFiles[i].PathLower < audioFiles[j].PathLower
			}
			return leftName < rightName
		}
		return audioFiles[i].ServerModified.Before(audioFiles[j].ServerModified)
	})

	return audioFiles, nil
}

func appendAudioFiles(dst *[]*files.FileMetadata, entries []files.IsMetadata) {
	for _, entry := range entries {
		file, ok := entry.(*files.FileMetadata)
		if !ok || !isAudioFile(file.Name) {
			continue
		}
		*dst = append(*dst, file)
	}
}

func isAudioFile(fileName string) bool {
	lowerName := strings.ToLower(fileName)
	return strings.HasSuffix(lowerName, ".mp3") ||
		strings.HasSuffix(lowerName, ".m4a") ||
		strings.HasSuffix(lowerName, ".m4b")
}

func buildItems(dbxs sharing.Client, audioFiles []*files.FileMetadata) ([]Item, error) {
	items := make([]Item, 0, len(audioFiles))
	for _, file := range audioFiles {
		sharedLink, err := getOrCreateSharedLink(dbxs, file.PathLower)
		if err != nil {
			return nil, fmt.Errorf("get shared link for %s: %w", file.Name, err)
		}

		downloadURL := sharedLinkToDownloadURL(sharedLink)
		enclosureType := "audio/x-m4a"
		if strings.HasSuffix(strings.ToLower(file.Name), ".mp3") {
			enclosureType = "audio/mpeg"
		}

		items = append(items, Item{
			Title:   file.Name,
			Link:    downloadURL,
			GUID:    downloadURL,
			PubDate: file.ServerModified.Format("Mon, 02 Jan 2006 15:04:05 -0700"),
			Enclosure: Enclosure{
				URL:    downloadURL,
				Length: int64(file.Size),
				Type:   enclosureType,
			},
			//ItunesDuration: formatDuration(duration),
		})
	}

	return items, nil
}

func getOrCreateSharedLink(dbxs sharing.Client, path string) (string, error) {
	sharedLink, found, err := lookupDirectSharedLink(dbxs, path)
	if err != nil {
		return "", err
	}
	if found {
		return sharedLink, nil
	}

	createArg := sharing.NewCreateSharedLinkWithSettingsArg(path)
	link, err := withRetry("create shared link", func() (sharing.IsSharedLinkMetadata, error) {
		return dbxs.CreateSharedLinkWithSettings(createArg)
	})
	if err != nil {
		apiError, ok := err.(dropbox.APIError)
		if ok && strings.HasPrefix(apiError.ErrorSummary, "shared_link_already_exists") {
			sharedLink, found, listErr := lookupDirectSharedLink(dbxs, path)
			if listErr != nil {
				return "", listErr
			}
			if found {
				return sharedLink, nil
			}
		}
		return "", err
	}

	return sharedLinkURL(link)
}

func lookupDirectSharedLink(dbxs sharing.Client, path string) (string, bool, error) {
	listArg := sharing.NewListSharedLinksArg()
	listArg.Path = path
	listArg.DirectOnly = true

	links, err := withRetry("list shared links", func() (*sharing.ListSharedLinksResult, error) {
		return dbxs.ListSharedLinks(listArg)
	})
	if err != nil {
		return "", false, err
	}
	if len(links.Links) == 0 {
		return "", false, nil
	}

	url, err := sharedLinkURL(links.Links[0])
	if err != nil {
		return "", false, err
	}
	return url, true, nil
}

func sharedLinkURL(link sharing.IsSharedLinkMetadata) (string, error) {
	sharedLink, ok := link.(*sharing.SharedLinkMetadata)
	if !ok {
		return "", fmt.Errorf("unexpected shared link metadata type %T", link)
	}
	return sharedLink.Url, nil
}

func sharedLinkToDownloadURL(sharedLink string) string {
	parsed, err := url.Parse(sharedLink)
	if err != nil {
		downloadURL := strings.Replace(sharedLink, "www.dropbox.com", "dl.dropboxusercontent.com", 1)
		return strings.Replace(downloadURL, "dl=0", "dl=1", 1)
	}

	if parsed.Host == "www.dropbox.com" {
		parsed.Host = "dl.dropboxusercontent.com"
	}
	query := parsed.Query()
	query.Set("dl", "1")
	parsed.RawQuery = query.Encode()

	return parsed.String()
}

func withRetry[T any](operation string, fn func() (T, error)) (T, error) {
	var zero T
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}

		lastErr = err
		if attempt == maxRetries {
			break
		}

		log.Printf("%s failed (attempt %d/%d): %v", operation, attempt, maxRetries, err)
		time.Sleep(time.Duration(attempt) * retryDelay)
	}

	return zero, fmt.Errorf("%s: %w", operation, lastErr)
}

func getAudioDurationFromDropbox(dbxf files.Client, path string) (time.Duration, error) {
	downloadArg := files.NewDownloadArg(path)
	_, content, err := dbxf.Download(downloadArg)
	if err != nil {
		return 0, err
	}
	defer content.Close()

	tmpfile, err := ioutil.TempFile("", "dircast-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmpfile.Name())

	data, err := ioutil.ReadAll(content)
	if err != nil {
		return 0, err
	}

	if _, err := tmpfile.Write(data); err != nil {
		return 0, err
	}

	cmd := exec.Command("ffprobe", "-v", "error", "-show_format", "-of", "json", tmpfile.Name())
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}

	var ffprobeOutput FFProbeOutput
	if err := json.Unmarshal(output, &ffprobeOutput); err != nil {
		return 0, err
	}

	duration, err := strconv.ParseFloat(ffprobeOutput.Format.Duration, 64)
	if err != nil {
		return 0, err
	}

	return time.Duration(duration * float64(time.Second)), nil
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// interactiveAuthFlow launches an authorization flow if no refresh token was provided.
// It prints the authorize URL, waits for the user to paste the code, then exchanges it.
func interactiveAuthFlow() (string, error) {
	appKey := os.Getenv("DROPBOX_APP_KEY")
	appSecret := os.Getenv("DROPBOX_APP_SECRET")
	if appKey == "" || appSecret == "" {
		return "", fmt.Errorf("missing DROPBOX_APP_KEY or DROPBOX_APP_SECRET in environment")
	}
	// 'code' flow recommended for server-side obtaining refresh token
	authorizeURL := fmt.Sprintf("https://www.dropbox.com/oauth2/authorize?response_type=code&token_access_type=offline&client_id=%s", url.QueryEscape(appKey))
	fmt.Println("Open this URL in your browser, authorize the app, then paste the returned code here:")
	fmt.Println(authorizeURL)
	fmt.Print("Authorization code: ")
	var code string
	if _, err := fmt.Scanln(&code); err != nil {
		return "", fmt.Errorf("reading authorization code: %w", err)
	}
	// Exchange code for tokens
	form := url.Values{}
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	req, err := http.NewRequest("POST", "https://api.dropbox.com/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(appKey, appSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %s: %s", resp.Status, string(body))
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("decode token JSON: %w", err)
	}
	if tr.RefreshToken == "" {
		return "", fmt.Errorf("no refresh_token in response; ensure 'token_access_type=offline' was used")
	}
	return tr.RefreshToken, nil
}

// fetchAccessToken exchanges a long-lived refresh token for a short-lived access token.
// Requires environment variables DROPBOX_APP_KEY and DROPBOX_APP_SECRET to be set.
func fetchAccessToken(refreshToken string) (string, error) {
	appKey := os.Getenv("DROPBOX_APP_KEY")
	appSecret := os.Getenv("DROPBOX_APP_SECRET")
	if appKey == "" || appSecret == "" {
		return "", fmt.Errorf("missing DROPBOX_APP_KEY or DROPBOX_APP_SECRET in environment")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequest("POST", "https://api.dropbox.com/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(appKey, appSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %s: %s", resp.Status, string(body))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
		UID         string `json:"uid"`
		AccountID   string `json:"account_id"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("decode token JSON: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("empty access_token in response")
	}
	return tr.AccessToken, nil
}
