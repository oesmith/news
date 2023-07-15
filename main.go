package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/mmcdole/gofeed"
)

var feedsPath = flag.String("feeds", "feeds.json", "File containing feed URLs")
var maxArticles = flag.Int("max_articles", 50, "Maximum number of articles to display per page")
var outputPath = flag.String("output", "html", "Path to output rendered HTML")
var timeout = flag.Duration("timeout", 30 * time.Second, "Fetch timeout")
var verbose = flag.Bool("verbose", false, "Enable verbose logging")

//go:embed template.html.tmpl
var pageTemplate string

var funcs = template.FuncMap{
	"inc": func(i int) int {
		return i + 1
	},
}

func main() {
	flag.Parse()

	tmpl, err := template.New("template").Funcs(funcs).Parse(pageTemplate)
	if err != nil {
		log.Fatal(err)
	}

	feeds, err := loadFeeds()
	if err != nil {
		log.Fatal(err)
	}

	if err := os.MkdirAll(*outputPath, 0755); err != nil {
		log.Fatal(err)
	}

	f := newFetcher()
	cachePath := filepath.Join(*outputPath, "cache.json")
	if err := f.loadCache(cachePath); err != nil {
		log.Print(err)
	}

	parser := gofeed.NewParser()

	sanitizer := bluemonday.NewPolicy()
	sanitizer.AllowURLSchemes("mailto", "http", "https")
	sanitizer.AllowElements("h3", "h4", "figure", "a", "p", "b", "i", "em", "strong", "blockquote", "ul", "ol", "li", "dl", "df", "dd", "sup", "sub")
	sanitizer.AllowAttrs("href").OnElements("a")
	sanitizer.RequireNoFollowOnLinks(true)
	sanitizer.AddTargetBlankToFullyQualifiedLinks(true)

	for _, page := range feeds.Pages {
		var articles timeSortableArticles

		for _, url := range page.Urls {
			data, err := f.get(url)
			if err != nil {
				log.Printf("%s (%s)", err, url)
				continue
			}

			feed, err := parser.ParseString(data)
			if err != nil {
				log.Printf("%s (%s)", err, url)
				continue
			}

			for _, i := range feed.Items {
				timestamp := i.PublishedParsed
				if timestamp == nil {
					timestamp = i.UpdatedParsed
				}
				formattedTime := ""
				if timestamp != nil {
					formattedTime = timestamp.Format("Monday 2 Jan 15:04")
				}
				content := sanitizer.Sanitize(i.Content)
				if content == "" {
					content = sanitizer.Sanitize(i.Description)
				}
				articles = append(articles, Article{
					Title:         i.Title,
					Content:       template.HTML(content),
					Url:           i.Link,
					Timestamp:     timestamp,
					FormattedTime: formattedTime,
					FeedTitle:     feed.Title,
					FeedUrl:       feed.Link,
				})
			}
		}

		sort.Sort(articles)

		if len(articles) > *maxArticles {
			articles = articles[:*maxArticles]
		}

		w, err := os.Create(filepath.Join(*outputPath, fmt.Sprintf("%s.html", page.Name)))
		if err != nil {
			log.Fatal(err)
		}
		defer w.Close()

		page := Page{
			Title:              page.Title,
			Name:               page.Name,
			FormattedFetchTime: time.Now().Format("Monday 2 Jan 15:04"),
			Pages:              feeds.Pages,
			Articles:           articles,
		}

		if err := tmpl.Execute(w, page); err != nil {
			log.Fatal(err)
		}
	}

	if err := f.saveCache(cachePath); err != nil {
		log.Print(err)
	}
}

type Feeds struct {
	Pages   []PageConfig `json:"pages"`
}

type PageConfig struct {
	Name  string   `json:"name"`
	Title string   `json:"title"`
	Urls  []string `json:"urls"`
}

func loadFeeds() (*Feeds, error) {
	b, err := os.ReadFile(*feedsPath)
	if err != nil {
		return nil, err
	}
	var f Feeds
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

type Page struct {
	Title              string
	Name               string
	FormattedFetchTime string
	Pages              []PageConfig
	Articles           []Article
}

type Article struct {
	Title         string
	Content       template.HTML
	Url           string
	Timestamp     *time.Time
	FormattedTime string
	FeedTitle     string
	FeedUrl       string
}

type timeSortableArticles []Article

func (a timeSortableArticles) Len() int {
	return len(a)
}

func (a timeSortableArticles) Less(i, j int) bool {
	if a[i].Timestamp == nil && a[j].Timestamp == nil {
		return false
	}
	return a[j].Timestamp == nil || a[j].Timestamp.Before(*a[i].Timestamp)
}

func (a timeSortableArticles) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

type Fetcher struct {
	client  *http.Client
	cache map[string]CacheEntry
}

type CacheEntry struct {
	Body         string    `json:"body"`
	LastModified string    `json:"last_modified"`
	ETag         string    `json:"etag"`
	Timestamp    time.Time `json:"timestamp"`
}

func newFetcher() *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: *timeout,
		},
		cache: make(map[string]CacheEntry),
	}
}

func (f *Fetcher) loadCache(filename string) error {
	cacheBytes, err := os.ReadFile(filepath.Join(*outputPath, "cache.json"))
	if err == nil {
		if err := json.Unmarshal(cacheBytes, &f.cache); err != nil {
			return fmt.Errorf("Failed to parse cache %s %v", filename, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("Failed to read cache %s %v", filename, err)
	}
	return nil
}

func (f *Fetcher) saveCache(filename string) error {
	cacheBytes, err := json.Marshal(f.cache)
	if err != nil {
		return fmt.Errorf("Failed to serialise cache %v", err)
	}
	if err := os.WriteFile(filename, cacheBytes, 0600); err != nil {
		return fmt.Errorf("Failed to save cache %s %v", filename, err)
	}
	return nil
}

func (f *Fetcher) get(url string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}

	ce, ok := f.cache[url]
	if ok {
		if ce.ETag != "" {
			req.Header.Add("If-None-Match", ce.ETag)
		}
		if ce.LastModified != "" {
			req.Header.Add("If-Modified-Since", ce.LastModified)
		}
	} else {
		ce = CacheEntry{}
	}

	res, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if *verbose {
		log.Printf("DEBUG %s (%s)", res.Status, url)
	}

	if res.StatusCode == http.StatusNotModified {
		return ce.Body, nil
	} else if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf(res.Status)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	s := string(body)

	ce.Body = s
	ce.ETag = res.Header.Get("ETag")
	ce.LastModified = res.Header.Get("Last-Modified")
	ce.Timestamp = time.Now()

	f.cache[url] = ce

	return s, nil
}
