package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
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

func main() {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		log.Fatal("ffprobe not found in PATH. Please install ffmpeg.")
	}

	if len(os.Args) < 5 {
		fmt.Println("Usage: go run main.go <dropboxToken> <dropboxPath> <baseURL> <imageURL>")
		os.Exit(1)
	}
	dropboxToken := os.Args[1]
	dropboxPath := os.Args[2]
	baseURL := os.Args[3]
	imageURL := os.Args[4]

	// Remove trailing slash from dropbox path
	dropboxPath = strings.TrimSuffix(dropboxPath, "/")

	config := dropbox.Config{
		Token: dropboxToken,
	}
	dbxf := files.New(config)
	dbxs := sharing.New(config)

	listFolderArg := files.NewListFolderArg(dropboxPath)
	listFolderResult, err := dbxf.ListFolder(listFolderArg)
	if err != nil {
		log.Fatalf("Failed to list files in Dropbox: %s", err)
	}

	var items []Item
	for _, entry := range listFolderResult.Entries {
		if file, ok := entry.(*files.FileMetadata); ok {
			fileName := file.Name
			if strings.HasSuffix(fileName, ".mp3") || strings.HasSuffix(fileName, ".m4a") || strings.HasSuffix(fileName, ".m4b") {
				sharedLink, err := getOrCreateSharedLink(dbxs, file.PathLower)
				if err != nil {
					log.Printf("Failed to create or get shared link for %s: %s", fileName, err)
					continue
				}

				downloadURL := strings.Replace(sharedLink, "www.dropbox.com", "dl.dropboxusercontent.com", 1)
				downloadURL = strings.Replace(downloadURL, "dl=0", "dl=1", 1)

				/*
					duration, err := getAudioDurationFromDropbox(dbxf, file.PathLower)
					if err != nil {
						log.Printf("Failed to get duration for %s: %s", fileName, err)
					}*/

				enclosureType := ""
				if strings.HasSuffix(fileName, ".mp3") {
					enclosureType = "audio/mpeg"
				} else {
					enclosureType = "audio/x-m4a"
				}

				item := Item{
					Title:   fileName,
					Link:    downloadURL,
					GUID:    downloadURL,
					PubDate: file.ServerModified.Format("Mon, 02 Jan 2006 15:04:05 -0700"),
					Enclosure: Enclosure{
						URL:    downloadURL,
						Length: int64(file.Size),
						Type:   enclosureType,
					},
					//ItunesDuration: formatDuration(duration),
				}
				items = append(items, item)
			}
		}
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

func getOrCreateSharedLink(dbxs sharing.Client, path string) (string, error) {
	arg := sharing.NewCreateSharedLinkArg(path)
	link, err := dbxs.CreateSharedLink(arg)
	if err != nil {
		apiError, ok := err.(dropbox.APIError)
		if ok && strings.HasPrefix(apiError.ErrorSummary, "shared_link_already_exists") {
			listArg := sharing.NewListSharedLinksArg()
			listArg.Path = path
			links, err := dbxs.ListSharedLinks(listArg)
			if err != nil {
				return "", err
			}
			if len(links.Links) > 0 {
				if sl, ok := links.Links[0].(*sharing.SharedLinkMetadata); ok {
					return sl.Url, nil
				}
			}
		}
		return "", err
	}
	return link.Url, nil
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
