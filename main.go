// main.go
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// -------- Data structures --------
type NewsResult struct {
	Title  string `json:"title"`
	URL    string `json:"url"`
	Source string `json:"source"`
}

type CachedSearch struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt `gorm:"index"`

	Query    string
	Days     int
	MaxItems int
	Title    string
	URL      string
	Created  time.Time
}

type NewsAPIResponse struct {
	Status       string `json:"status"`
	TotalResults int    `json:"totalResults"`
	Articles     []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"articles"`
}

// -------- Task structures --------
type Task struct {
	Query    string
	Days     int
	MaxItems int
	Resp     chan TaskResult
	Ctx      context.Context
}

type TaskResult struct {
	Results []NewsResult
	Source  string
	Err     error
}

// -------- DB helpers --------
func openDB(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&CachedSearch{}); err != nil {
		return nil, err
	}
	return db, nil
}

func fetchNewsAPI(query string, days, maxItems int) ([]NewsResult, error) {
	apiKey := os.Getenv("NEWSAPI_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("NEWSAPI_KEY not set")
	}
	fromDate := time.Now().AddDate(0, 0, -days+1).Format("2006-01-02")
	url := fmt.Sprintf("https://newsapi.org/v2/everything?q=%s&from=%s&pageSize=%d&apiKey=%s", query, fromDate, maxItems, apiKey)

	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result NewsAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	news := []NewsResult{}
	for _, a := range result.Articles {
		news = append(news, NewsResult{Title: a.Title, URL: a.URL, Source: "API"})
		if len(news) >= maxItems {
			break
		}
	}
	return news, nil
}

func getCachedResults(db *gorm.DB, query string, days, maxItems int) []NewsResult {
	var cached []CachedSearch
	db.Where("query = ? AND days >= ? AND max_items >= ?", query, days, maxItems).Order("created desc").Find(&cached)
	results := []NewsResult{}
	for _, c := range cached {
		results = append(results, NewsResult{Title: c.Title, URL: c.URL, Source: "DB"})
		if len(results) >= maxItems {
			break
		}
	}
	return results
}

func getMaxCachedParams(db *gorm.DB, query string) (int, int) {
	var cached CachedSearch
	tx := db.Where("query = ?", query).Order("days desc, max_items desc").First(&cached)
	if tx.Error != nil || tx.RowsAffected == 0 {
		return 0, 0
	}
	return cached.Days, cached.MaxItems
}

func storeFetched(db *gorm.DB, query string, days, maxItems int, results []NewsResult) {
	for _, r := range results {
		db.Create(&CachedSearch{
			Query:    query,
			Days:     days,
			MaxItems: maxItems,
			Title:    r.Title,
			URL:      r.URL,
			Created:  time.Now(),
		})
	}
}

// -------- Worker pool --------
func startWorkerPool(db *gorm.DB, workers int, tasks <-chan Task, wg *sync.WaitGroup) {
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasks {
				select {
				case <-t.Ctx.Done():
					t.Resp <- TaskResult{Results: nil, Source: "", Err: fmt.Errorf("request canceled")}
					continue
				default:
				}

				maxDaysCached, maxItemsCached := getMaxCachedParams(db, t.Query)
				var final []NewsResult
				var src string

				if maxDaysCached >= t.Days && maxItemsCached >= t.MaxItems {
					final = getCachedResults(db, t.Query, t.Days, t.MaxItems)
					src = "DB"
				} else {
					fetched, err := fetchNewsAPI(t.Query, t.Days, t.MaxItems)
					if err != nil {
						final = getCachedResults(db, t.Query, t.Days, t.MaxItems)
						if len(final) > 0 {
							src = "DB"
						} else {
							t.Resp <- TaskResult{Results: nil, Source: "", Err: err}
							continue
						}
					} else {
						storeFetched(db, t.Query, t.Days, t.MaxItems, fetched)
						final = getCachedResults(db, t.Query, t.Days, t.MaxItems)
						src = "API"
					}
				}
				t.Resp <- TaskResult{Results: final, Source: src, Err: nil}
			}
		}()
	}
}

// -------- CLI helpers --------
func readUsersFile(filename string) ([]struct {
	Topic    string
	Days     int
	MaxItems int
}, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var topics []struct {
		Topic    string
		Days     int
		MaxItems int
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 3 {
			fmt.Println("Skipping invalid line in input file:", line)
			continue
		}
		days, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
		maxItems, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
		topics = append(topics, struct {
			Topic    string
			Days     int
			MaxItems int
		}{Topic: strings.TrimSpace(parts[0]), Days: days, MaxItems: maxItems})
	}
	return topics, scanner.Err()
}

func runCLI(tasks chan<- Task, inputFileName string) {
	// Input path
	inputFile := filepath.Join("Inputs(Sampel Testcases)", inputFileName)

	// Ensure Outputs folder exists
	os.MkdirAll("Outputs", os.ModePerm)

	reader := bufio.NewReader(os.Stdin)

	for {
		userTopics, err := readUsersFile(inputFile)
		if err != nil {
			fmt.Println("Error reading input file:", err)
			return
		}

		resultsMap := make(map[string]TaskResult)
		var mu sync.Mutex
		var wgLocal sync.WaitGroup

		for _, ut := range userTopics {
			wgLocal.Add(1)
			go func(u struct {
				Topic          string
				Days, MaxItems int
			}) {
				defer wgLocal.Done()
				respCh := make(chan TaskResult, 1)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()

				task := Task{
					Query:    u.Topic,
					Days:     u.Days,
					MaxItems: u.MaxItems,
					Resp:     respCh,
					Ctx:      ctx,
				}

				select {
				case tasks <- task:
				case <-ctx.Done():
					respCh <- TaskResult{Results: nil, Source: "", Err: fmt.Errorf("timeout submitting task")}
				}

				res := <-respCh
				mu.Lock()
				resultsMap[u.Topic] = res
				mu.Unlock()
			}(ut)
		}

		wgLocal.Wait()

		// Output file automatically named after input file in Outputs folder
		baseName := strings.TrimSuffix(inputFileName, ".txt")
		outFile := filepath.Join("Outputs", fmt.Sprintf("Outputs_%s.txt", baseName))
		file, err := os.Create(outFile)
		if err != nil {
			fmt.Println("Error writing output file:", err)
			return
		}
		w := bufio.NewWriter(file)

		for _, u := range userTopics {
			r := resultsMap[u.Topic]
			if r.Err != nil {
				w.WriteString(fmt.Sprintf("Results for \"%s\" (error: %v)\n\n", u.Topic, r.Err))
				continue
			}
			w.WriteString(fmt.Sprintf("Results for \"%s\" (Fetched from: %s):\n", u.Topic, r.Source))
			if len(r.Results) == 0 {
				w.WriteString("- No results found\n\n")
			} else {
				for _, res := range r.Results {
					w.WriteString(fmt.Sprintf("- %s (%s)\n", res.Title, res.URL))
				}
				w.WriteString("\n")
			}
		}

		w.Flush()
		file.Close()

		fmt.Printf("Execution completed. Results stored in %s\n", outFile)
		fmt.Print("Press Enter to run again, or type 'exit' to quit: ")
		input, _ := reader.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(input)) == "exit" {
			fmt.Println("Exiting program")
			return
		}
	}
}

// -------- main --------
func main() {
	// Change this variable to run a different input file
	inputFile := "user10.txt"

	db, err := openDB("news_cache.db")
	if err != nil {
		log.Fatalf("failed to open db: %v", err)
	}

	taskQueue := make(chan Task, 1000)
	var workersWg sync.WaitGroup
	startWorkerPool(db, 8, taskQueue, &workersWg)

	runCLI(taskQueue, inputFile)

	close(taskQueue)
	workersWg.Wait()
}
