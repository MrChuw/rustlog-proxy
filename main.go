package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/joho/godotenv"
)

type Handlers struct {
	recentURL  *url.URL
	rustlogURL *url.URL
	rustlogAPI string
	client     *http.Client
}

var (
	addedMu      sync.Mutex
	alreadyAdded = make(map[string]struct{}) // IDs already added to rustlog
	inProgress   = make(map[string]struct{}) // IDs currently being posted (to avoid duplicate posts)

	ErrUnmarshalFailed = errors.New("failed to decode JSON")
	ErrNoMessages      = errors.New("no 'messages' field found")
	ErrInvalidMessages = errors.New("'messages' field is not a slice")
	ErrNoRoomIDFound   = errors.New("no 'room-id' found in messages")

	roomIDRegex       = regexp.MustCompile(`room-id=(\d+);`)
	defaultListenAddr = ":18080"
	upstreamTimeout   = 10 * time.Second
	logLoc            *time.Location
)

const (
	Reset  = "\033[0m"
	Bold   = "\033[1m"
	Faint  = "\033[2m"
	Italic = "\033[3m"
	Red    = "\033[31m"
	Green  = "\033[32m"
)

type logWriter struct{}

func (writer logWriter) Write(bytes []byte) (int, error) {
	timestamp := time.Now().Format("2006-01-02T15:04:05.999Z")
	formattedLog := fmt.Sprintf("%s%s%s %s", Faint, timestamp, Reset, string(bytes))
	return fmt.Print(formattedLog)
}

func LogInfo(format string, args ...interface{}) string {
	msg := fmt.Sprintf(format, args...)
	return fmt.Sprintf("%s%sINFO%s %s", Green, Bold, Reset, msg)
}

func LogError(format string, args ...interface{}) string {
	msg := fmt.Sprintf(format, args...)
	return fmt.Sprintf("%s%sERROR%s %s", Red, Bold, Reset, msg)
}

func LogFatal(format string, args ...interface{}) string {
	msg := fmt.Sprintf(format, args...)
	return fmt.Sprintf("%s%sFATAL%s %s", Red, Bold, Reset, msg)
}

func LogDebug(format string, args ...interface{}) string {
	msg := fmt.Sprintf(format, args...)
	return fmt.Sprintf("%s%sDEBUG%s %s", Faint, Bold, Reset, msg)
}

func init() {
	_ = godotenv.Load()
	log.SetFlags(0)
	log.SetOutput(new(logWriter))
}

func main() {
	recentURLStr := os.Getenv("RECENT_URL")
	rustlogURLStr := os.Getenv("RUSTLOG_URL")
	rustlogAPI := os.Getenv("RUSTLOG_API")
	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}

	if recentURLStr == "" || rustlogURLStr == "" || rustlogAPI == "" {
		log.Println(LogFatal("RECENT_URL, RUSTLOG_URL, and RUSTLOG_API must be set. RECENT_URL=%q RUSTLOG_URL=%q RUSTLOG_API=%q",
			recentURLStr, rustlogURLStr, rustlogAPI))
	}

	recentURL, err := url.Parse(recentURLStr)
	if err != nil {
		log.Println(LogFatal("RECENT_URL invalid: %v", err))
	}
	rustlogURL, err := url.Parse(rustlogURLStr)
	if err != nil {
		log.Println(LogFatal("RUSTLOG_URL invalid: %v", err))
	}

	h := &Handlers{
		recentURL:  recentURL,
		rustlogURL: rustlogURL,
		rustlogAPI: rustlogAPI,
		client: &http.Client{
			Timeout: upstreamTimeout,
		},
	}

	r := chi.NewRouter()
	r.Get("/recent-messages/{channel_login}", h.getRecentMessages)

	srv := &http.Server{
		Addr:    listenAddr,
		Handler: r,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println(LogInfo("Signal received: starting graceful shutdown..."))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Println(LogFatal("Shutdown error: %v", err))
		}
		close(idleConnsClosed)
	}()

	if err := fetchAndAddRustlogIDs(rustlogURLStr); err != nil {
		<-idleConnsClosed
		log.Println(LogInfo("Server stopped due to error: %v", err))
		log.Println(LogInfo("Server stopped."))
	}
	log.Println(LogInfo("Server started on %s", listenAddr))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Println(LogFatal("ListenAndServe error: %v", err))
	}
	<-idleConnsClosed
	log.Println(LogInfo("Server stopped."))
}

func (h *Handlers) getRecentMessages(w http.ResponseWriter, r *http.Request) {
	var infos []string
	addInfo := func(format string, args ...interface{}) {
		infos = append(infos, fmt.Sprintf(format, args...))
	}
	defer func() {
		if len(infos) == 0 {
			return
		}
		combined := strings.Join(infos, " | ")
		log.Println(LogInfo("%s", combined))
	}()

	channelLogin := chi.URLParam(r, "channel_login")
	addInfo("Channel: %s", channelLogin)

	finalURL := *h.recentURL
	finalURL = *finalURL.JoinPath(channelLogin)

	reqQuery := r.URL.Query()
	if len(reqQuery) > 0 {
		finalQuery := finalURL.Query()
		for k, vals := range reqQuery {
			for _, v := range vals {
				finalQuery.Add(k, v)
			}
		}
		finalURL.RawQuery = finalQuery.Encode()
	}

	ctx := r.Context()
	upstreamReq, err := http.NewRequestWithContext(ctx, "GET", finalURL.String(), nil)
	if err != nil {
		http.Error(w, "Error building upstream request", http.StatusInternalServerError)
		log.Println(LogError("Error creating upstream request: %v", err))
		return
	}

	upstreamReq.Header.Set("User-Agent", "rustlog-proxy/0.1")

	upstreamResp, err := h.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "Error contacting external API.", http.StatusBadGateway)
		log.Println(LogError("HTTP client error for %s: %v", finalURL.String(), err))
		return
	}
	defer upstreamResp.Body.Close()

	bodyBytes, err := io.ReadAll(upstreamResp.Body)
	if err != nil {
		http.Error(w, "Error reading API response body.", http.StatusInternalServerError)
		log.Println(LogError("Error reading body: %v", err))
		return
	}

	roomID, parseErr := parseMessages(bodyBytes)
	if parseErr != nil {
		switch {
		case errors.Is(parseErr, ErrNoMessages):
			addInfo("parseMessages: JSON does not contain 'messages'.")
		case errors.Is(parseErr, ErrInvalidMessages):
			addInfo("parseMessages: 'messages' is not valid.")
		case errors.Is(parseErr, ErrNoRoomIDFound):
			addInfo("parseMessages: no room-id found.")
		default:
			log.Println(LogError("parseMessages: unexpected error: %v", parseErr))
			addInfo("parseMessages: unexpected error: %v", parseErr)
		}
	} else {
		addInfo("parsed roomID: %s", roomID)
	}

	if roomID != "" {
		shouldPost := false

		addedMu.Lock()
		if _, ok := alreadyAdded[roomID]; ok {
			addInfo("roomID %s already added; skipping.", roomID)
		} else if _, ok := inProgress[roomID]; ok {
			addInfo("roomID %s is already in progress; skipping.", roomID)
		} else {
			inProgress[roomID] = struct{}{}
			shouldPost = true
			addInfo("roomID %s not present; attempting POST.", roomID)
		}
		addedMu.Unlock()

		if shouldPost {
			payload := map[string][]string{"channels": {roomID}}
			jsonData, err := json.Marshal(payload)
			if err != nil {
				log.Println(LogError("Error preparing payload for rustlog: %v", err))
				addInfo("Error preparing payload for rustlog: %v", err)
				addedMu.Lock()
				delete(inProgress, roomID)
				addedMu.Unlock()
			} else {
				postURL := *h.rustlogURL
				postURL.Path = "/admin/channels"
				postReq, err := http.NewRequestWithContext(ctx, "POST", postURL.String(), bytes.NewBuffer(jsonData))
				if err != nil {
					log.Println(LogError("Error creating request to rustlog: %v", err))
					addInfo("Error creating request to rustlog: %v", err)
					addedMu.Lock()
					delete(inProgress, roomID)
					addedMu.Unlock()
				} else {
					postReq.Header.Set("Content-Type", "application/json")
					postReq.Header.Set("X-Api-Key", h.rustlogAPI)
					log.Println(postReq.URL)

					resp, err := h.client.Do(postReq)
					if err != nil {
						log.Println(LogError("Error posting to rustlog for roomID %s: %v", roomID, err))
						addInfo("Error posting to rustlog for roomID %s: %v", roomID, err)
						addedMu.Lock()
						delete(inProgress, roomID)
						addedMu.Unlock()
					} else {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						addInfo("rustlog %s response, status: %s", roomID, resp.Status)

						if resp.StatusCode >= 200 && resp.StatusCode < 300 {
							addedMu.Lock()
							alreadyAdded[roomID] = struct{}{}
							delete(inProgress, roomID)
							addedMu.Unlock()
							addInfo("roomID %s successfully added.", roomID)
						} else {
							addInfo("rustlog non-200 for roomID %s; will allow future retries.", roomID)
							addedMu.Lock()
							delete(inProgress, roomID)
							addedMu.Unlock()
						}
					}
				}
			}
		}
	}

	contentType := upstreamResp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(upstreamResp.StatusCode)

	if _, err := w.Write(bodyBytes); err != nil {
		log.Println(LogError("Error writing response to client: %v", err))
	}
}

func parseMessages(reader []byte) (string, error) {
	var response map[string]interface{}
	if err := json.Unmarshal(reader, &response); err != nil {
		log.Println(LogError("Error decoding JSON: %v", err))
		return "", fmt.Errorf("%w: %v", ErrUnmarshalFailed, err)
	}

	messagesInterface, ok := response["messages"]
	if !ok {
		return "", ErrNoMessages
	}

	messagesSlice, ok := messagesInterface.([]interface{})
	if !ok {
		return "", ErrInvalidMessages
	}
	for _, item := range messagesSlice {
		// If item is string
		if s, ok := item.(string); ok {
			if m := roomIDRegex.FindStringSubmatch(s); len(m) >= 2 {
				return m[1], nil
			}
			continue
		}
		if mObj, ok := item.(map[string]interface{}); ok {
			for _, key := range []string{"message", "text", "body"} {
				if val, exists := mObj[key]; exists {
					if s, ok := val.(string); ok {
						if m := roomIDRegex.FindStringSubmatch(s); len(m) >= 2 {
							return m[1], nil
						}
					}
				}
			}
			conc := flattenStringsFromMap(mObj)
			if conc != "" {
				if m := roomIDRegex.FindStringSubmatch(conc); len(m) >= 2 {
					return m[1], nil
				}
			}
		}
	}

	return "", ErrNoRoomIDFound
}

func flattenStringsFromMap(m map[string]interface{}) string {
	var parts []string
	for _, v := range m {
		switch t := v.(type) {
		case string:
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

func fetchAndAddRustlogIDs(rustlogURLStr string) error {
	resp, err := http.Get(rustlogURLStr + "/channels")
	if err != nil {
		return fmt.Errorf("rustlog request error: %w", err)
	}
	defer resp.Body.Close()

	var data struct {
		Channels []struct {
			Name   string `json:"name"`
			UserID string `json:"userID"`
		} `json:"channels"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return fmt.Errorf("error decoding JSON: %w", err)
	}
	var userIDs []string
	for _, ch := range data.Channels {
		alreadyAdded[ch.UserID] = struct{}{}
		userIDs = append(userIDs, ch.UserID)
	}

	if len(userIDs) > 0 {
		log.Println(LogInfo("User Ids already present in rustlog: %s", strings.Join(userIDs, ", ")))
	}

	return nil
}
