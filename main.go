package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/gorilla/websocket"
	"github.com/rivo/tview"
)

// Config holds the bot's configuration
type Config struct {
	Username        string   `json:"username"`
	Password        string   `json:"password"`
	IntervalSeconds int      `json:"intervalSeconds"`
	Messages        []string `json:"messages"`
	BaseURL         string   `json:"baseUrl,omitempty"`
	WebSocketURL    string   `json:"webSocketUrl,omitempty"`
}

// TUI handles the terminal user interface
type TUI struct {
	app          *tview.Application
	chatView     *tview.TextView
	logView      *tview.TextView
	userView     *tview.TextView
	inputField   *tview.InputField
	statusBar    *tview.TextView
	layout       *tview.Flex
	chatMutex    sync.Mutex
	logMutex     sync.Mutex
	userMutex    sync.Mutex
	users        map[string]bool
	currentRoom  string
}

// ChatBot represents the chat bot instance
type ChatBot struct {
	username        string
	password        string
	baseURL         string
	wsURL           string
	httpClient      *http.Client
	wsConn          *websocket.Conn
	cookies         []*http.Cookie
	messageInterval time.Duration
	messages        []string
	currentMsgIndex int
	wsMutex         sync.Mutex
	currentRoom     string
	messageTimer    *time.Timer
	timerMutex      sync.Mutex
	tui             *TUI
}

// NewTUI creates a new terminal user interface
func NewTUI() *TUI {
	tui := &TUI{
		app:   tview.NewApplication(),
		users: make(map[string]bool),
	}

	// Create the main components
	tui.chatView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true).
		SetChangedFunc(func() {
			tui.app.Draw()
		})
	tui.chatView.SetBorder(true).SetTitle(" Chat Messages ").SetTitleAlign(tview.AlignLeft)

	tui.logView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true).
		SetChangedFunc(func() {
			tui.app.Draw()
		})
	tui.logView.SetBorder(true).SetTitle(" System Log ").SetTitleAlign(tview.AlignLeft)

	tui.userView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetChangedFunc(func() {
			tui.app.Draw()
		})
	tui.userView.SetBorder(true).SetTitle(" Users ").SetTitleAlign(tview.AlignLeft)

	tui.inputField = tview.NewInputField().
		SetLabel("> ").
		SetFieldWidth(0).
		SetFieldBackgroundColor(tcell.ColorBlack).
		SetFieldTextColor(tcell.ColorWhite)
	tui.inputField.SetBorder(true).SetTitle(" Input ").SetTitleAlign(tview.AlignLeft)

	tui.statusBar = tview.NewTextView().
		SetDynamicColors(true).
		SetText("Starting Skyskraber Chat Bot...")
	tui.statusBar.SetBorder(true).SetTitle(" Status ").SetTitleAlign(tview.AlignLeft)

	// Create layout
	mainContent := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(tui.chatView, 0, 3, false).      // Left pane (chat) - 3/5 of width
		AddItem(tui.logView, 0, 2, false).       // Right pane (logs) - 2/5 of width
		AddItem(tui.userView, 20, 0, false)      // Most right pane (users) - fixed 20 chars

	bottomSection := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tui.inputField, 3, 0, true).     // Input field - 3 lines high
		AddItem(tui.statusBar, 3, 0, false)      // Status bar - 3 lines high

	tui.layout = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(mainContent, 0, 1, false).       // Main content area
		AddItem(bottomSection, 6, 0, true)       // Bottom section - 6 lines total

	// Enable mouse support
	tui.app.EnableMouse(true)
	tui.app.SetRoot(tui.layout, true)

	return tui
}

// AddChatMessage adds a formatted chat message to the chat pane
func (tui *TUI) AddChatMessage(timestamp, username, message string) {
	tui.app.QueueUpdateDraw(func() {
		tui.chatMutex.Lock()
		defer tui.chatMutex.Unlock()

		timeStr := timestamp
		if timeStr == "" {
			timeStr = time.Now().Format("15:04:05")
		}

		var formattedMsg string
		if username != "" {
			formattedMsg = fmt.Sprintf("[gray]%s[white] [yellow]<%s>[white] %s\n", timeStr, username, message)
		} else {
			formattedMsg = fmt.Sprintf("[gray]%s[white] [blue]***[white] %s\n", timeStr, message)
		}

		fmt.Fprint(tui.chatView, formattedMsg)
		tui.chatView.ScrollToEnd()
	})
}

// AddLogMessage adds a log message to the log pane
func (tui *TUI) AddLogMessage(message string) {
	tui.app.QueueUpdateDraw(func() {
		tui.logMutex.Lock()
		defer tui.logMutex.Unlock()

		timeStr := time.Now().Format("15:04:05.000")
		formattedMsg := fmt.Sprintf("[gray]%s[white] %s\n", timeStr, message)
		
		fmt.Fprint(tui.logView, formattedMsg)
		tui.logView.ScrollToEnd()
	})
}

// UpdateUsers updates the user list in the user pane
func (tui *TUI) UpdateUsers(users []string) {
	tui.app.QueueUpdateDraw(func() {
		tui.userMutex.Lock()
		defer tui.userMutex.Unlock()

		tui.userView.Clear()
		fmt.Fprintf(tui.userView, "[yellow]Room: %s[white]\n\n", tui.currentRoom)
		
		for _, user := range users {
			fmt.Fprintf(tui.userView, "[green]● [white]%s\n", user)
		}
	})
}

// AddUser adds a user to the current room
func (tui *TUI) AddUser(username string) {
	tui.userMutex.Lock()
	tui.users[username] = true
	users := make([]string, 0, len(tui.users))
	for user := range tui.users {
		users = append(users, user)
	}
	tui.userMutex.Unlock()
	
	tui.UpdateUsers(users)
}

// RemoveUser removes a user from the current room
func (tui *TUI) RemoveUser(username string) {
	tui.userMutex.Lock()
	delete(tui.users, username)
	users := make([]string, 0, len(tui.users))
	for user := range tui.users {
		users = append(users, user)
	}
	tui.userMutex.Unlock()
	
	tui.UpdateUsers(users)
}

// SetRoom updates the current room and clears users
func (tui *TUI) SetRoom(roomID, roomName string) {
	tui.userMutex.Lock()
	tui.currentRoom = fmt.Sprintf("%s (%s)", roomName, roomID)
	tui.users = make(map[string]bool) // Clear users when changing rooms
	tui.userMutex.Unlock()
	
	tui.UpdateUsers([]string{})
}

// SetRoomUsers sets the initial user list for a room
func (tui *TUI) SetRoomUsers(usernames []string) {
	tui.userMutex.Lock()
	tui.users = make(map[string]bool)
	for _, username := range usernames {
		tui.users[username] = true
	}
	tui.userMutex.Unlock()
	
	tui.UpdateUsers(usernames)
}

// UpdateStatus updates the status bar
func (tui *TUI) UpdateStatus(status string) {
	tui.app.QueueUpdateDraw(func() {
		tui.statusBar.Clear()
		timeStr := time.Now().Format("15:04:05")
		fmt.Fprintf(tui.statusBar, "[gray]%s[white] %s", timeStr, status)
	})
}

// Run starts the TUI application
func (tui *TUI) Run() error {
	return tui.app.Run()
}

// Stop stops the TUI application
func (tui *TUI) Stop() {
	tui.app.Stop()
}

// loadConfig loads configuration from a JSON file or creates a template
func loadConfig(configPath string) (*Config, error) {
	defaultConfig := &Config{
		Username:        "CHANGE_THIS_USERNAME",
		Password:        "CHANGE_THIS_PASSWORD",
		IntervalSeconds: 300,
		Messages: []string{
			"ADD_YOUR_MESSAGES_HERE",
			"EXAMPLE: Hej alle!",
			"EXAMPLE: Hvordan har I det?",
		},
		BaseURL:      "https://www.skyskraber.dk",
		WebSocketURL: "wss://www.skyskraber.dk/ws",
	}

	configFile, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			configData, errJson := json.MarshalIndent(defaultConfig, "", "  ")
			if errJson != nil {
				return nil, fmt.Errorf("failed to marshal template config: %v", errJson)
			}

			if errWrite := os.WriteFile(configPath, configData, 0644); errWrite != nil {
				return nil, fmt.Errorf("failed to create template config file: %v", errWrite)
			}

			return defaultConfig, nil
		}
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var config Config
	if errJson := json.Unmarshal(configFile, &config); errJson != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", errJson)
	}

	if config.BaseURL == "" {
		config.BaseURL = "https://www.skyskraber.dk"
	}
	if config.WebSocketURL == "" {
		config.WebSocketURL = "wss://www.skyskraber.dk/ws"
	}

	return &config, nil
}

// NewChatBot creates a new ChatBot instance
func NewChatBot(config *Config, tui *TUI) *ChatBot {
	return &ChatBot{
		username:        config.Username,
		password:        config.Password,
		baseURL:         config.BaseURL,
		wsURL:           config.WebSocketURL,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		messageInterval: time.Duration(config.IntervalSeconds) * time.Second,
		currentRoom:     "1",
		messages:        config.Messages,
		currentMsgIndex: 0,
		tui:             tui,
	}
}

// log is a helper function to log to the TUI instead of stdout
func (bot *ChatBot) log(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	if bot.tui != nil {
		bot.tui.AddLogMessage(message)
	}
}

// login handles the authentication process
func (bot *ChatBot) login() error {
	bot.log("🔐 Starting login process...")

	bot.log("📄 Getting login page: %s/chat", bot.baseURL)
	loginPageResp, err := bot.httpClient.Get(bot.baseURL + "/chat")
	if err != nil {
		return fmt.Errorf("failed to get login page: %v", err)
	}
	defer loginPageResp.Body.Close()
	
	bot.log("📄 Login page response status: %d", loginPageResp.StatusCode)
	bot.cookies = loginPageResp.Cookies()
	bot.log("🍪 Cookies after GET /chat: %d", len(bot.cookies))

	loginData := map[string]string{"username": bot.username, "password": bot.password}
	jsonData, err := json.Marshal(loginData)
	if err != nil {
		return fmt.Errorf("failed to marshal login data: %v", err)
	}

	req, err := http.NewRequest("POST", bot.baseURL+"/api/auth/login", strings.NewReader(string(jsonData)))
	if err != nil {
		return fmt.Errorf("failed to create login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", bot.baseURL+"/chat")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	
	for _, cookie := range bot.cookies {
		req.AddCookie(cookie)
	}

	bot.log("📤 Sending JSON POST request to /api/auth/login")
	resp, err := bot.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	bot.log("📥 Login POST response status: %d", resp.StatusCode)
	bot.log("📥 Login POST response body: %s", string(respBody))

	currentResponseCookies := resp.Cookies()

	if resp.StatusCode == 200 || resp.StatusCode == 201 {
		var loginResponse map[string]interface{}
		if errJson := json.Unmarshal(respBody, &loginResponse); errJson == nil {
			if id, ok := loginResponse["id"]; ok {
				bot.log("✅ Login successful! User ID: %v, Username: %v", id, loginResponse["username"])
				bot.cookies = currentResponseCookies
				bot.log("🍪 Overwritten bot.cookies with %d cookies from successful login POST.", len(bot.cookies))
				return nil
			}
			if success, ok := loginResponse["success"].(bool); ok && success {
				bot.log("✅ Login successful via success field!")
				bot.cookies = currentResponseCookies
				bot.log("🍪 Overwritten bot.cookies with %d cookies from successful login POST (success field).", len(bot.cookies))
				return nil
			}
		}
		bot.log("✅ Login assumed successful based on status %d", resp.StatusCode)
		bot.cookies = currentResponseCookies
		bot.log("🍪 Overwritten bot.cookies with %d cookies from assumed successful login POST.", len(bot.cookies))
		return nil
	} else if resp.StatusCode == 302 {
		bot.log("✅ Login successful (redirect response %d)", resp.StatusCode)
		bot.cookies = currentResponseCookies
		bot.log("🍪 Overwritten bot.cookies with %d cookies from successful redirect login POST.", len(bot.cookies))
		return nil
	}

	return fmt.Errorf("login failed with status: %d, body: %s", resp.StatusCode, string(respBody))
}

// connectWebSocket establishes the WebSocket connection
func (bot *ChatBot) connectWebSocket() error {
	bot.log("🔌 Starting WebSocket connection...")
	headers := http.Header{}
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	headers.Set("Origin", bot.baseURL)

	finalCookiesForHeaderMap := make(map[string]string)
	for _, cookie := range bot.cookies {
		finalCookiesForHeaderMap[cookie.Name] = cookie.Value
	}

	if len(finalCookiesForHeaderMap) > 0 {
		var cookieStrings []string
		for name, value := range finalCookiesForHeaderMap {
			cookieStrings = append(cookieStrings, fmt.Sprintf("%s=%s", name, value))
		}
		headers.Set("Cookie", strings.Join(cookieStrings, "; "))
		bot.log("🍪 WebSocket Cookie header (uniqued names by map): %s", headers.Get("Cookie"))
	} else {
		bot.log("⚠️ No cookies to send for WebSocket header")
	}

	var latestSessionCookieForURLFallback *http.Cookie
	for i := len(bot.cookies) - 1; i >= 0; i-- {
		cookie := bot.cookies[i]
		if cookie.Name == "session" || cookie.Name == "connect.sid" || cookie.Name == "sessionid" || cookie.Name == "PHPSESSID" {
			latestSessionCookieForURLFallback = cookie
			valPreview := cookie.Value
			if len(valPreview) > 10 {
				valPreview = valPreview[:10] + "..."
			}
			bot.log("🍪 Identified latest session cookie for potential URL fallback: %s (value: %s)", cookie.Name, valPreview)
			break
		}
	}

	bot.log("🔌 Connecting to: %s", bot.wsURL)
	dialer := websocket.Dialer{HandshakeTimeout: 45 * time.Second}
	conn, resp, err := dialer.Dial(bot.wsURL, headers)

	if err != nil {
		errMsg := fmt.Sprintf("failed to connect to WebSocket (initial attempt): %v", err)
		if resp != nil {
			errMsg += fmt.Sprintf(", status: %d", resp.StatusCode)
			bodyBytes, _ := io.ReadAll(resp.Body)
			errMsg += fmt.Sprintf(", response: %s", string(bodyBytes))
			resp.Body.Close()
		}
		bot.log("❌ %s", errMsg)

		if latestSessionCookieForURLFallback != nil {
			wsURLWithToken := fmt.Sprintf("%s?session_token=%s", bot.wsURL, latestSessionCookieForURLFallback.Value)
			bot.log("🔄 Trying WebSocket connection with LATEST session token in URL")
			conn, resp, err = dialer.Dial(wsURLWithToken, headers)
			if err == nil {
				bot.log("✅ WebSocket connected with session token in URL!")
			} else {
				errMsgToken := fmt.Sprintf("failed to connect with token in URL: %v", err)
				if resp != nil {
					errMsgToken += fmt.Sprintf(", status: %d", resp.StatusCode)
					bodyBytes, _ := io.ReadAll(resp.Body)
					errMsgToken += fmt.Sprintf(", response: %s", string(bodyBytes))
					resp.Body.Close()
				}
				bot.log("❌ %s", errMsgToken)
				return fmt.Errorf(errMsgToken)
			}
		} else {
			bot.log("⚠️ No session cookie identified to attempt URL token fallback.")
			return fmt.Errorf(errMsg)
		}
	}

	if resp != nil {
		bot.log("✅ WebSocket handshake successful! Status: %d", resp.StatusCode)
		resp.Body.Close()
	}
	bot.wsConn = conn
	bot.log("✅ WebSocket connected successfully!")
	return nil
}

// sendMessageInternal sends a message over WebSocket and optionally resets the periodic timer
func (bot *ChatBot) sendMessageInternal(message string, resetTimer bool) error {
	bot.wsMutex.Lock()
	defer bot.wsMutex.Unlock()

	if bot.wsConn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	bot.log("💬 Preparing to send message to room %s: '%s'", bot.currentRoom, message)

	msgPayload := map[string]interface{}{
		"type": "chat",
		"data": map[string]interface{}{
			"message": message,
			"room":    bot.currentRoom,
		},
	}

	msgBytes, err := json.Marshal(msgPayload)
	if err != nil {
		bot.log("❌ Failed to marshal message: %v. Payload: %+v", err, msgPayload)
		return fmt.Errorf("failed to marshal message: %v", err)
	}

	bot.log("📤 Sending message: %s", string(msgBytes))
	
	// Set a write deadline to prevent hanging
	bot.wsConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		bot.log("❌ Failed to send message: %v", err)
		return fmt.Errorf("failed to send message: %v", err)
	}

	if resetTimer {
		bot.resetMessageTimer()
		bot.log("⏰ Periodic message timer reset after sending manual message.")
	}

	bot.log("✅ Message sent successfully: '%s'", message)
	return nil
}

// sendMessage is a convenience wrapper for sending periodic messages
func (bot *ChatBot) sendMessage(message string) error {
	return bot.sendMessageInternal(message, false)
}

// sendConsoleMessage is for messages typed by user
func (bot *ChatBot) sendConsoleMessage(message string) error {
	err := bot.sendMessageInternal(message, true)
	if err == nil {
		// Show the message in chat pane as well (in goroutine to avoid blocking)
		go func() {
			bot.tui.AddChatMessage("", bot.username, message)
		}()
	}
	return err
}

// resetMessageTimer stops and resets the periodic message timer
func (bot *ChatBot) resetMessageTimer() {
	bot.timerMutex.Lock()
	defer bot.timerMutex.Unlock()

	if bot.messageTimer != nil {
		if !bot.messageTimer.Stop() {
			select {
			case <-bot.messageTimer.C:
			default:
			}
		}
		bot.messageTimer.Reset(bot.messageInterval)
		bot.log("⏰ Message timer reset to %v", bot.messageInterval)
	} else {
		bot.log("⚠️ Tried to reset a nil message timer.")
	}
}

// sendAcknowledgment sends a specific acknowledgment message to the server
func (bot *ChatBot) sendAcknowledgment(messageType string) error {
	bot.wsMutex.Lock()
	defer bot.wsMutex.Unlock()

	if bot.wsConn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	bot.log("ℹ️ Preparing to send acknowledgment for server event: '%s'", messageType)

	var ackPayload interface{}

	switch messageType {
	case "newHour":
		ackPayload = map[string]interface{}{
			"type": "hour",
		}
		bot.log("✅ Using specific acknowledgment payload for '%s'", messageType)
	default:
		bot.log("⚠️ No specific acknowledgment logic defined for messageType: '%s'", messageType)
		return fmt.Errorf("no specific acknowledgment handler defined for messageType: %s", messageType)
	}

	msgBytes, err := json.Marshal(ackPayload)
	if err != nil {
		bot.log("❌ Failed to marshal acknowledgment payload for '%s': %v", messageType, err)
		return fmt.Errorf("failed to marshal acknowledgment payload for '%s': %v", messageType, err)
	}

	bot.log("📤 Sending acknowledgment for '%s': %s", messageType, string(msgBytes))
	bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		bot.log("❌ Failed to send acknowledgment for '%s': %v", messageType, err)
		return fmt.Errorf("failed to send acknowledgment for '%s': %v", messageType, err)
	}

	bot.log("✅ Acknowledgment for '%s' sent successfully.", messageType)
	return nil
}

// listenForMessages reads messages from WebSocket and processes them
func (bot *ChatBot) listenForMessages() {
	bot.log("👂 Starting message listener...")
	for {
		if bot.wsConn == nil {
			bot.log("⚠️ WebSocket connection is nil in listener, waiting for reconnect...")
			time.Sleep(2 * time.Second)
			continue
		}

		_, message, err := bot.wsConn.ReadMessage()
		if err != nil {
			bot.log("❌ Error reading message: %v", err)
			if bot.wsConn != nil {
				bot.wsConn.Close()
			}
			bot.wsConn = nil

			bot.log("🔄 Connection lost, attempting to reconnect WebSocket...")
			time.Sleep(5 * time.Second)
			if errConnect := bot.connectWebSocket(); errConnect != nil {
				bot.log("❌ Reconnection failed: %v. Will retry...", errConnect)
			} else {
				bot.log("✅ Reconnected successfully after connection loss!")
			}
			continue
		}

		bot.log("📥 Raw message received: %s", string(message))

		var messageData map[string]interface{}
		if errJson := json.Unmarshal(message, &messageData); errJson == nil {
			// Handle room changes and initial room data
			if roomInfo, ok := messageData["room"].(map[string]interface{}); ok {
				if idVal, idOk := roomInfo["id"]; idOk {
					newRoomID := fmt.Sprintf("%v", idVal)
					if nameVal, nameOk := roomInfo["name"].(string); nameOk {
						if bot.currentRoom != newRoomID {
							bot.log("🚪 Room changed by server: %s -> %s (%s)", bot.currentRoom, newRoomID, nameVal)
							bot.currentRoom = newRoomID
							bot.tui.SetRoom(newRoomID, nameVal)
						}
					}
				}

				// Parse initial client list when joining room
				if clientsData, clientsOk := messageData["clients"].(map[string]interface{}); clientsOk {
					if updates, updatesOk := clientsData["updates"].([]interface{}); updatesOk {
						var users []string
						for _, update := range updates {
							if client, clientOk := update.(map[string]interface{}); clientOk {
								if username, usernameOk := client["username"].(string); usernameOk {
									users = append(users, username)
								}
							}
						}
						bot.tui.SetRoomUsers(users)
						bot.log("👥 Found %d users in room", len(users))
					}
				}
			}

			// Handle client updates (user joins)
			if clientsData, ok := messageData["clients"].(map[string]interface{}); ok {
				// Handle user joins (updates)
				if updates, updatesOk := clientsData["updates"].([]interface{}); updatesOk {
					for _, update := range updates {
						if client, clientOk := update.(map[string]interface{}); clientOk {
							if username, usernameOk := client["username"].(string); usernameOk {
								// Check if this is a new user join (not initial room data)
								if _, hasRoom := messageData["room"]; !hasRoom {
									bot.tui.AddUser(username)
									bot.log("👤 User joined: %s", username)
								}
							}
						}
					}
				}

				// Handle user leaves/disconnects (removes)
				if _, removesOk := clientsData["removes"].([]interface{}); removesOk {
					// We need to track user IDs to usernames to handle removes properly
					// For now, we'll parse the system message to get the username
					if player, playerOk := messageData["player"].(map[string]interface{}); playerOk {
						if events, eventsOk := player["events"].([]interface{}); eventsOk {
							for _, event := range events {
								if eventData, eventOk := event.(map[string]interface{}); eventOk {
									if eventType, typeOk := eventData["type"].(string); typeOk && eventType == "system" {
										if data, dataOk := eventData["data"].(map[string]interface{}); dataOk {
											if message, msgOk := data["message"].(string); msgOk {
												// Parse system messages like "XXX gik til Receptionen" or "XXX mistede forbindelsen"
												if strings.Contains(message, "gik til") || strings.Contains(message, "mistede forbindelsen") {
													parts := strings.Fields(message)
													if len(parts) > 0 {
														username := parts[0]
														bot.tui.RemoveUser(username)
														bot.tui.AddChatMessage("", "", message)
														bot.log("👤 User left/disconnected: %s", username)
													}
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}

			// Handle system events
			if player, hasPlayer := messageData["player"].(map[string]interface{}); hasPlayer {
				// Handle newHour events
				if newHour, hasNewHour := player["newHour"].(bool); hasNewHour && newHour {
					bot.log("🕐 'newHour' event detected from server - sending acknowledgment...")
					go func() {
						if errAck := bot.sendAcknowledgment("newHour"); errAck != nil {
							bot.log("❌ Failed to acknowledge 'newHour' event: %v", errAck)
						}
					}()
				}

				// Handle system events like user arrivals
				if events, eventsOk := player["events"].([]interface{}); eventsOk {
					for _, event := range events {
						if eventData, eventOk := event.(map[string]interface{}); eventOk {
							if eventType, typeOk := eventData["type"].(string); typeOk && eventType == "system" {
								if data, dataOk := eventData["data"].(map[string]interface{}); dataOk {
									if message, msgOk := data["message"].(string); msgOk {
										// Handle user arrival messages
										if strings.Contains(message, "ankom til rummet") {
											parts := strings.Fields(message)
											if len(parts) > 0 {
												username := parts[0]
												bot.tui.AddUser(username)
												bot.tui.AddChatMessage("", "", message)
												bot.log("👤 User arrived: %s", username)
											}
										} else if strings.Contains(message, "Du er ankommet til") {
											// Handle own arrival message
											bot.tui.AddChatMessage("", "", message)
										}
									}
								}
							}
						}
					}
				}
			}

			// Handle chat messages - display in chat pane
			if msgType, ok := messageData["type"].(string); ok && (msgType == "chat" || msgType == "message") {
				if data, dataOk := messageData["data"].(map[string]interface{}); dataOk {
					username, _ := data["username"].(string)
					chatMsg, _ := data["message"].(string)
					if username != "" && chatMsg != "" {
						bot.log("💬 Chat [%s]: %s", username, chatMsg)
						bot.tui.AddChatMessage("", username, chatMsg)
					}
				}
			}
			if user, userOk := messageData["user"].(string); userOk {
				if text, textOk := messageData["text"].(string); textOk {
					bot.log("💬 Chat [%s]: %s", user, text)
					bot.tui.AddChatMessage("", user, text)
				}
			}
		} else {
			bot.log("⚠️ Could not parse message as JSON: %v. Message: %s", errJson, string(message))
		}
	}
}

// sendPeriodicMessages sends messages from the configured list at intervals
func (bot *ChatBot) sendPeriodicMessages() {
	if len(bot.messages) == 0 || bot.messages[0] == "ADD_YOUR_MESSAGES_HERE" {
		bot.log("⚠️ No valid periodic messages configured. Periodic sender will not run.")
		return
	}
	bot.log("🕐 Starting periodic message sender (interval: %v)", bot.messageInterval)

	bot.timerMutex.Lock()
	if bot.messageTimer == nil {
		bot.messageTimer = time.NewTimer(bot.messageInterval)
	} else {
		bot.messageTimer.Reset(bot.messageInterval)
	}
	bot.timerMutex.Unlock()

	for {
		bot.timerMutex.Lock()
		timerC := bot.messageTimer.C
		bot.timerMutex.Unlock()

		<-timerC

		if bot.wsConn != nil && len(bot.messages) > 0 {
			message := bot.messages[bot.currentMsgIndex]
			bot.log("⏰ Time to send periodic message [%d/%d]: '%s'", bot.currentMsgIndex+1, len(bot.messages), message)
			if err := bot.sendMessage(message); err != nil {
				bot.log("❌ Failed to send periodic message: %v", err)
			} else {
				bot.log("✅ Periodic message sent successfully: '%s'", message)
				// Show the message in chat as well
				bot.tui.AddChatMessage("", bot.username, message)
			}
			bot.currentMsgIndex = (bot.currentMsgIndex + 1) % len(bot.messages)
		} else if bot.wsConn == nil {
			bot.log("⚠️ Skipping periodic message - WebSocket not connected.")
		}

		bot.timerMutex.Lock()
		if bot.messageTimer != nil {
			bot.messageTimer.Reset(bot.messageInterval)
		}
		bot.timerMutex.Unlock()
	}
}

// setupInputHandler configures the input field for commands and messages
func (bot *ChatBot) setupInputHandler() {
	bot.tui.inputField.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			input := strings.TrimSpace(bot.tui.inputField.GetText())
			if input == "" {
				return
			}

			// Clear input field immediately for better UX
			bot.tui.inputField.SetText("")

			// Handle commands and messages in goroutine to not block TUI
			go func(message string) {
				// Handle commands
				if strings.HasPrefix(message, "/") {
					command := strings.Fields(message)[0]

					switch command {
					case "/quit", "/exit", "/q":
						bot.log("⌨️ Quit command received. Shutting down...")
						bot.Stop()
						bot.tui.app.Stop()
						os.Exit(0)
					case "/help", "/h":
						bot.tui.AddChatMessage("", "", "Available Commands:")
						bot.tui.AddChatMessage("", "", "/quit, /exit, /q - Exit the bot")
						bot.tui.AddChatMessage("", "", "/help, /h - Show this help")
						bot.tui.AddChatMessage("", "", "/status - Show bot status")
						bot.tui.AddChatMessage("", "", "/clear - Clear chat history")
					case "/status":
						wsStatus := "Disconnected"
						if bot.wsConn != nil {
							wsStatus = "Connected"
						}
						bot.tui.AddChatMessage("", "", fmt.Sprintf("Username: %s", bot.username))
						bot.tui.AddChatMessage("", "", fmt.Sprintf("Current Room: %s", bot.currentRoom))
						bot.tui.AddChatMessage("", "", fmt.Sprintf("WebSocket: %s", wsStatus))
						bot.tui.AddChatMessage("", "", fmt.Sprintf("Message Interval: %v", bot.messageInterval))
					case "/clear":
						bot.tui.app.QueueUpdateDraw(func() {
							bot.tui.chatView.Clear()
						})
					default:
						// Send command as message
						bot.log("⌨️ Sending command as message: '%s'", message)
						if err := bot.sendConsoleMessage(message); err != nil {
							bot.log("❌ Failed to send console message: %v", err)
						}
					}
				} else {
					// Send regular message
					bot.log("⌨️ Sending message: '%s'", message)
					if err := bot.sendConsoleMessage(message); err != nil {
						bot.log("❌ Failed to send console message: %v", err)
					}
				}
			}(input)
		}
	})

	// Set input field as focusable and ensure it gets focus
	bot.tui.app.SetFocus(bot.tui.inputField)
}

// sendKeepAlive sends WebSocket ping messages periodically
func (bot *ChatBot) sendKeepAlive() {
	bot.log("💓 Starting keep-alive sender (30 second interval)")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if bot.wsConn != nil {
			bot.wsMutex.Lock()
			bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := bot.wsConn.WriteMessage(websocket.PingMessage, []byte("keepalive")); err != nil {
				bot.log("❌ Failed to send ping: %v", err)
			}
			bot.wsMutex.Unlock()
		}
	}
}

// Start initializes and runs the bot
func (bot *ChatBot) Start() error {
	bot.log("🚀 Starting Skyskraber chat bot...")
	bot.tui.UpdateStatus("Starting bot...")

	bot.log("🔐 Step 1: Authenticating...")
	bot.tui.UpdateStatus("Authenticating...")
	if err := bot.login(); err != nil {
		return fmt.Errorf("login failed: %v", err)
	}

	bot.log("🔌 Step 2: Establishing WebSocket connection...")
	bot.tui.UpdateStatus("Connecting to WebSocket...")
	if err := bot.connectWebSocket(); err != nil {
		return fmt.Errorf("WebSocket connection failed: %v", err)
	}

	bot.log("👂 Step 3: Starting message listener...")
	bot.tui.UpdateStatus("Starting message listener...")
	go bot.listenForMessages()

	bot.log("🕐 Step 4: Starting periodic message sender...")
	bot.tui.UpdateStatus("Starting periodic message sender...")
	go bot.sendPeriodicMessages()

	bot.log("💓 Step 5: Starting keep-alive routine...")
	bot.tui.UpdateStatus("Starting keep-alive...")
	go bot.sendKeepAlive()

	bot.log("⌨️ Step 6: Setting up input handler...")
	bot.setupInputHandler()

	bot.log("✅ Bot started successfully! 🎉")
	bot.tui.UpdateStatus(fmt.Sprintf("Connected as %s | Room: %s | Type /help for commands", bot.username, bot.currentRoom))
	
	bot.tui.AddChatMessage("", "", "=== Skyskraber Chat Bot Started ===")
	bot.tui.AddChatMessage("", "", "Type /help for available commands")
	bot.tui.AddChatMessage("", "", "Press Ctrl+C or type /quit to exit")
	
	return nil
}

// Stop gracefully shuts down the bot
func (bot *ChatBot) Stop() {
	bot.log("🛑 Stopping bot...")

	bot.timerMutex.Lock()
	if bot.messageTimer != nil {
		bot.messageTimer.Stop()
		bot.log("⏰ Periodic message timer stopped.")
	}
	bot.timerMutex.Unlock()

	if bot.wsConn != nil {
		bot.log("🔌 Closing WebSocket connection...")
		err := bot.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			bot.log("⚠️ Error sending close message: %v", err)
		}
		bot.wsConn.Close()
		bot.wsConn = nil
		bot.log("🔌 WebSocket connection closed.")
	}
	bot.log("✅ Bot stopped.")
}

// main is the entry point of the application
func main() {
	config, err := loadConfig("config.json")
	if err != nil {
		fmt.Printf("❌ CRITICAL: Failed to load config: %v\n", err)
		os.Exit(1)
	}

	if strings.Contains(config.Username, "CHANGE_THIS") ||
		strings.Contains(config.Password, "CHANGE_THIS") ||
		(len(config.Messages) > 0 && strings.Contains(config.Messages[0], "ADD_YOUR_MESSAGES")) {
		fmt.Println("🚨 CONFIGURATION REQUIRED:")
		fmt.Println("Please edit 'config.json' and update:")
		fmt.Println("- username: your actual username")
		fmt.Println("- password: your actual password")
		fmt.Println("- messages: your custom messages array")
		fmt.Println("Then run the bot again!")
		os.Exit(1)
	}

	// Handle command line arguments
	if len(os.Args) > 1 {
		config.Username = os.Args[1]
	}
	if len(os.Args) > 2 {
		config.Password = os.Args[2]
	}
	if len(os.Args) > 3 {
		if interval, errAtoi := strconv.Atoi(os.Args[3]); errAtoi == nil && interval > 0 {
			config.IntervalSeconds = interval
		}
	}

	// Create TUI and bot
	tui := NewTUI()
	botInstance := NewChatBot(config, tui)

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		botInstance.log("🛑 Received signal: %v. Initiating graceful shutdown...", sig)
		botInstance.Stop()
		tui.app.Stop()
		os.Exit(0)
	}()

	// Start the bot in a goroutine
	go func() {
		// Add a small delay to ensure TUI is ready
		time.Sleep(500 * time.Millisecond)
		if errStart := botInstance.Start(); errStart != nil {
			tui.AddLogMessage(fmt.Sprintf("❌ CRITICAL: Failed to start bot: %v", errStart))
		}
	}()

	// Set focus to input field and ensure it's active
	tui.app.SetFocus(tui.inputField)

	// Run the TUI (this blocks until the application exits)
	if err := tui.Run(); err != nil {
		fmt.Printf("❌ TUI Error: %v\n", err)
		os.Exit(1)
	}
}
