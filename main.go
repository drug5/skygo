package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
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
	wsMutex         sync.Mutex  // Prevent concurrent WebSocket writes
	currentRoom     string      // Track current room ID, updated by server messages
	messageTimer    *time.Timer // Timer for periodic messages
	timerMutex      sync.Mutex  // Protect timer operations
}

// loadConfig loads configuration from a JSON file or creates a template
func loadConfig(configPath string) (*Config, error) {
	// Default configuration with placeholders
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
			log.Printf("📄 Config file '%s' not found", configPath)
			log.Printf("🔧 Creating template config file...")

			configData, errJson := json.MarshalIndent(defaultConfig, "", "  ")
			if errJson != nil {
				return nil, fmt.Errorf("failed to marshal template config: %v", errJson)
			}

			if errWrite := os.WriteFile(configPath, configData, 0644); errWrite != nil {
				return nil, fmt.Errorf("failed to create template config file: %v", errWrite)
			}

			log.Printf("✅ Created '%s' template file", configPath)
			log.Printf("🚨 REQUIRED: Edit '%s' and configure all settings!", configPath)
			log.Printf("💡 Then restart the bot to use your configuration")
			return defaultConfig, nil // Return default config so program can exit gracefully if needed
		}
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var config Config
	if errJson := json.Unmarshal(configFile, &config); errJson != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", errJson)
	}

	// Set defaults for optional fields if they are empty
	if config.BaseURL == "" {
		config.BaseURL = "https://www.skyskraber.dk"
	}
	if config.WebSocketURL == "" {
		config.WebSocketURL = "wss://www.skyskraber.dk/ws"
	}

	// Validate required fields
	if config.Username == "" || strings.Contains(config.Username, "CHANGE_THIS") {
		log.Printf("⚠️  Please update the username in '%s'", configPath)
	}
	if config.Password == "" || strings.Contains(config.Password, "CHANGE_THIS") {
		log.Printf("⚠️  Please update the password in '%s'", configPath)
	}
	if len(config.Messages) == 0 || (len(config.Messages) > 0 && strings.Contains(config.Messages[0], "ADD_YOUR_MESSAGES")) {
		log.Printf("⚠️  Please update the messages in '%s'", configPath)
	}
	if config.IntervalSeconds <= 0 { // Ensure interval is positive
		log.Printf("⚠️  IntervalSeconds must be positive, defaulting to 300. Original value: %d", config.IntervalSeconds)
		config.IntervalSeconds = 300
	}

	log.Printf("📄 Config loaded from '%s'", configPath)
	return &config, nil
}

// NewChatBot creates a new ChatBot instance
func NewChatBot(config *Config) *ChatBot {
	return &ChatBot{
		username:        config.Username,
		password:        config.Password,
		baseURL:         config.BaseURL,
		wsURL:           config.WebSocketURL,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		messageInterval: time.Duration(config.IntervalSeconds) * time.Second,
		currentRoom:     "1", // Default to Receptionen, will be updated by server
		messages:        config.Messages,
		currentMsgIndex: 0,
	}
}

// login handles the authentication process
func (bot *ChatBot) login() error {
	log.Println("🔐 Starting login process...")

	// Get login page to potentially pick up initial cookies/CSRF
	log.Printf("📄 Getting login page: %s/chat", bot.baseURL)
	loginPageResp, err := bot.httpClient.Get(bot.baseURL + "/chat")
	if err != nil {
		return fmt.Errorf("failed to get login page: %v", err)
	}
	defer loginPageResp.Body.Close()
	log.Printf("📄 Login page response status: %d", loginPageResp.StatusCode)
	// Store cookies from initial GET. These might be overwritten if POST login is successful.
	bot.cookies = loginPageResp.Cookies()
	log.Printf("🍪 Cookies after GET /chat: %d", len(bot.cookies))


	// Prepare JSON login data
	loginData := map[string]string{"username": bot.username, "password": bot.password}
	jsonData, err := json.Marshal(loginData)
	if err != nil {
		return fmt.Errorf("failed to marshal login data: %v", err)
	}

	// Create POST request for login
	req, err := http.NewRequest("POST", bot.baseURL+"/api/auth/login", strings.NewReader(string(jsonData)))
	if err != nil {
		return fmt.Errorf("failed to create login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", bot.baseURL+"/chat")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	// Add cookies obtained from the initial GET request
	for _, cookie := range bot.cookies {
		req.AddCookie(cookie)
	}

	log.Printf("📤 Sending JSON POST request to /api/auth/login")
	resp, err := bot.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("📥 Login POST response status: %d", resp.StatusCode)
	log.Printf("📥 Login POST response body: %s", string(respBody))

	// Store cookies from THIS POST response.
	currentResponseCookies := resp.Cookies()

	if resp.StatusCode == 200 || resp.StatusCode == 201 {
		var loginResponse map[string]interface{}
		if errJson := json.Unmarshal(respBody, &loginResponse); errJson == nil {
			if id, ok := loginResponse["id"]; ok {
				log.Printf("✅ Login successful! User ID: %v, Username: %v", id, loginResponse["username"])
				bot.cookies = currentResponseCookies // IMPORTANT: Overwrite with cookies from this successful response
				log.Printf("🍪 Overwritten bot.cookies with %d cookies from successful login POST.", len(bot.cookies))
				return nil
			}
			if success, ok := loginResponse["success"].(bool); ok && success {
				log.Printf("✅ Login successful via success field!")
				bot.cookies = currentResponseCookies // IMPORTANT: Overwrite
				log.Printf("🍪 Overwritten bot.cookies with %d cookies from successful login POST (success field).", len(bot.cookies))
				return nil
			}
		}
		log.Printf("✅ Login assumed successful based on status %d (body: %s)", resp.StatusCode, string(respBody))
		bot.cookies = currentResponseCookies // IMPORTANT: Overwrite
		log.Printf("🍪 Overwritten bot.cookies with %d cookies from assumed successful login POST.", len(bot.cookies))
		return nil
	} else if resp.StatusCode == 302 {
		log.Printf("✅ Login successful (redirect response %d)", resp.StatusCode)
		bot.cookies = currentResponseCookies // IMPORTANT: Overwrite
		log.Printf("🍪 Overwritten bot.cookies with %d cookies from successful redirect login POST.", len(bot.cookies))
		return nil
	}

	return fmt.Errorf("login failed with status: %d, body: %s", resp.StatusCode, string(respBody))
}

// min is a helper function to find the minimum of two integers.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// connectWebSocket establishes the WebSocket connection
func (bot *ChatBot) connectWebSocket() error {
	log.Println("🔌 Starting WebSocket connection...")
	headers := http.Header{}
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	headers.Set("Origin", bot.baseURL)

	// 1. Construct the Cookie header from bot.cookies, ensuring unique names (last one wins).
	//    The login() fix should mean bot.cookies contains cookies from the successful POST.
	finalCookiesForHeaderMap := make(map[string]string)
	for _, cookie := range bot.cookies { // Iterate forwards, so later cookies with same name overwrite previous ones
		finalCookiesForHeaderMap[cookie.Name] = cookie.Value
	}

	if len(finalCookiesForHeaderMap) > 0 {
		var cookieStrings []string
		for name, value := range finalCookiesForHeaderMap {
			cookieStrings = append(cookieStrings, fmt.Sprintf("%s=%s", name, value))
		}
		headers.Set("Cookie", strings.Join(cookieStrings, "; "))
		log.Printf("🍪 WebSocket Cookie header (uniqued names by map): %s", headers.Get("Cookie"))
	} else {
		log.Println("⚠️ No cookies to send for WebSocket header (bot.cookies was empty or map creation failed).")
	}

	// 2. Identify the LATEST session cookie specifically for the URL token fallback
	var latestSessionCookieForURLFallback *http.Cookie
	for i := len(bot.cookies) - 1; i >= 0; i-- { // Iterate in reverse to find the latest
		cookie := bot.cookies[i]
		// Common session cookie names
		if cookie.Name == "session" || cookie.Name == "connect.sid" || cookie.Name == "sessionid" || cookie.Name == "PHPSESSID" {
			latestSessionCookieForURLFallback = cookie
			// Safe logging of cookie value (first 10 chars)
			valPreview := cookie.Value
			if len(valPreview) > 10 {
				valPreview = valPreview[:10] + "..."
			}
			log.Printf("🍪 Identified latest session cookie for potential URL fallback: %s (value: %s)", cookie.Name, valPreview)
			break // Found the latest one
		}
	}

	log.Printf("🔌 Connecting to: %s", bot.wsURL)
	dialer := websocket.Dialer{HandshakeTimeout: 45 * time.Second}
	conn, resp, err := dialer.Dial(bot.wsURL, headers) // Initial dial attempt

	if err != nil { // This block handles failure of the initial dial
		errMsg := fmt.Sprintf("failed to connect to WebSocket (initial attempt): %v", err)
		if resp != nil {
			errMsg += fmt.Sprintf(", status: %d", resp.StatusCode)
			bodyBytes, _ := io.ReadAll(resp.Body)
			errMsg += fmt.Sprintf(", response: %s", string(bodyBytes))
			resp.Body.Close() // Ensure body is closed
		}
		log.Printf("❌ %s", errMsg) // Log the error from the initial attempt

		// Attempt connection with session token in query parameter as a fallback
		if latestSessionCookieForURLFallback != nil {
			// Using a common query param name, adjust if known otherwise
			wsURLWithToken := fmt.Sprintf("%s?session_token=%s", bot.wsURL, latestSessionCookieForURLFallback.Value)
			log.Printf("🔄 Trying WebSocket connection with LATEST session token in URL: %s", wsURLWithToken)
			// Re-assign conn, resp, err for the new attempt
			conn, resp, err = dialer.Dial(wsURLWithToken, headers) // Pass original headers
			if err == nil {
				log.Printf("✅ WebSocket connected with session token in URL!")
				// Success, proceed to set bot.wsConn outside this if block
			} else {
				// Error from the token attempt
				errMsgToken := fmt.Sprintf("failed to connect with token in URL: %v", err)
				if resp != nil {
					errMsgToken += fmt.Sprintf(", status: %d", resp.StatusCode)
					bodyBytes, _ := io.ReadAll(resp.Body)
					errMsgToken += fmt.Sprintf(", response: %s", string(bodyBytes))
					resp.Body.Close() // Ensure body is closed
				}
				log.Printf("❌ %s", errMsgToken)
				return fmt.Errorf(errMsgToken) // Return the error from the token attempt
			}
		} else {
			log.Println("⚠️ No session cookie identified to attempt URL token fallback.")
			return fmt.Errorf(errMsg) // Return original error if no session token to try
		}
	}

	// This part is reached if either initial dial or fallback dial succeeded
	if resp != nil { // resp might be from initial or fallback dial
		log.Printf("✅ WebSocket handshake successful! Status: %d", resp.StatusCode)
		resp.Body.Close() // Close response body from successful handshake
	}
	bot.wsConn = conn
	log.Println("✅ WebSocket connected successfully!")
	return nil
}


// sendMessageInternal sends a message over WebSocket and optionally resets the periodic timer
func (bot *ChatBot) sendMessageInternal(message string, resetTimer bool) error {
	bot.wsMutex.Lock()
	defer bot.wsMutex.Unlock()

	if bot.wsConn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	log.Printf("💬 Preparing to send message to room %s: '%s'", bot.currentRoom, message)

	msgPayload := map[string]interface{}{
		"type": "chat",
		"data": map[string]interface{}{
			"message": message,
			"room":    bot.currentRoom,
		},
	}

	msgBytes, err := json.Marshal(msgPayload)
	if err != nil {
		log.Printf("❌ Failed to marshal message: %v. Payload: %+v", err, msgPayload)
		return fmt.Errorf("failed to marshal message: %v", err)
	}

	log.Printf("📤 Sending message: %s", string(msgBytes))
	err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		log.Printf("❌ Failed to send message: %v", err)
		return fmt.Errorf("failed to send message: %v", err)
	}

	if resetTimer {
		bot.resetMessageTimer()
		log.Printf("⏰ Periodic message timer reset after sending manual message.")
	}

	log.Printf("✅ Message sent successfully: '%s'", message)
	return nil
}

// sendMessage is a convenience wrapper for sending periodic messages (doesn't reset timer)
func (bot *ChatBot) sendMessage(message string) error {
	return bot.sendMessageInternal(message, false)
}

// sendConsoleMessage is for messages typed by user (resets periodic timer)
func (bot *ChatBot) sendConsoleMessage(message string) error {
	return bot.sendMessageInternal(message, true)
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
		log.Printf("⏰ Message timer reset to %v", bot.messageInterval)
	} else {
		log.Printf("⚠️ Tried to reset a nil message timer. This might happen during setup or shutdown.")
	}
}

// sendAcknowledgment sends a specific acknowledgment message to the server
func (bot *ChatBot) sendAcknowledgment(messageType string) error {
	bot.wsMutex.Lock()
	defer bot.wsMutex.Unlock()

	if bot.wsConn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	log.Printf("ℹ️ Preparing to send acknowledgment for server event: '%s'", messageType)

	var ackPayload interface{}

	switch messageType {
	case "newHour":
		ackPayload = map[string]interface{}{
			"type": "hour",
		}
		log.Printf("✅ Using specific acknowledgment payload for '%s'", messageType)
	default:
		log.Printf("⚠️ No specific acknowledgment logic defined for messageType: '%s'. Not sending acknowledgment.", messageType)
		return fmt.Errorf("no specific acknowledgment handler defined for messageType: %s", messageType)
	}

	msgBytes, err := json.Marshal(ackPayload)
	if err != nil {
		log.Printf("❌ Failed to marshal acknowledgment payload for '%s': %v. Payload: %+v", messageType, err, ackPayload)
		return fmt.Errorf("failed to marshal acknowledgment payload for '%s': %v", messageType, err)
	}

	log.Printf("📤 Sending acknowledgment for '%s': %s", messageType, string(msgBytes))
	err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		log.Printf("❌ Failed to send acknowledgment for '%s': %v", messageType, err)
		return fmt.Errorf("failed to send acknowledgment for '%s': %v", messageType, err)
	}

	log.Printf("✅ Acknowledgment for '%s' sent successfully.", messageType)
	return nil
}

// changeRoom sends a message to the server to change the chat room.
// NOTE: This function is currently not used by console commands.
func (bot *ChatBot) changeRoom(roomID string) error {
	bot.wsMutex.Lock()
	defer bot.wsMutex.Unlock()

	if bot.wsConn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	log.Printf("🚪 Attempting to send room change request for room: %s", roomID)

	roomChangePayload := map[string]interface{}{
		"type": "joinRoom",
		"data": map[string]interface{}{
			"roomId": roomID,
		},
	}

	msgBytes, err := json.Marshal(roomChangePayload)
	if err != nil {
		log.Printf("❌ Failed to marshal room change payload: %v. Payload: %+v", err, roomChangePayload)
		return fmt.Errorf("failed to marshal room change payload: %v", err)
	}

	log.Printf("📤 Sending room change request: %s", string(msgBytes))
	err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		log.Printf("❌ Failed to send room change request for room %s: %v", roomID, err)
		return fmt.Errorf("failed to send room change request for room %s: %v", roomID, err)
	}

	log.Printf("✅ Room change request for room %s sent successfully.", roomID)
	return nil
}


// listenForMessages reads messages from WebSocket and processes them
func (bot *ChatBot) listenForMessages() {
	log.Println("👂 Starting message listener...")
	for {
		if bot.wsConn == nil {
			log.Println("⚠️ WebSocket connection is nil in listener, waiting for reconnect...")
			time.Sleep(2 * time.Second)
			continue
		}

		_, message, err := bot.wsConn.ReadMessage()
		if err != nil {
			log.Printf("❌ Error reading message: %v", err)
			if bot.wsConn != nil { // Check wsConn before closing
				bot.wsConn.Close()
			}
			bot.wsConn = nil

			log.Println("🔄 Connection lost, attempting to reconnect WebSocket...")
			time.Sleep(5 * time.Second)
			if errConnect := bot.connectWebSocket(); errConnect != nil {
				log.Printf("❌ Reconnection failed: %v. Will retry...", errConnect)
			} else {
				log.Println("✅ Reconnected successfully after connection loss!")
			}
			continue
		}

		log.Printf("📥 Raw message received: %s", string(message))

		var messageData map[string]interface{}
		if errJson := json.Unmarshal(message, &messageData); errJson == nil {
			if roomInfo, ok := messageData["room"].(map[string]interface{}); ok {
				if idVal, idOk := roomInfo["id"]; idOk {
					newRoomID := fmt.Sprintf("%v", idVal)
					if nameVal, nameOk := roomInfo["name"].(string); nameOk {
						if bot.currentRoom != newRoomID {
							log.Printf("🚪 Room changed by server: %s -> %s (%s)", bot.currentRoom, newRoomID, nameVal)
							bot.currentRoom = newRoomID
						}
					}
				}
			} else if roomData, ok := messageData["data"].(map[string]interface{}); ok {
				if roomID, idExists := roomData["id"].(string); idExists {
					if roomName, nameExists := roomData["name"].(string); nameExists {
						if bot.currentRoom != roomID {
							log.Printf("🚪 Room changed by server (nested data): %s -> %s (%s)", bot.currentRoom, roomID, roomName)
							bot.currentRoom = roomID
						}
					}
				}
			}

			if player, hasPlayer := messageData["player"].(map[string]interface{}); hasPlayer {
				if newHour, hasNewHour := player["newHour"].(bool); hasNewHour && newHour {
					log.Printf("🕐 'newHour' event detected from server - sending acknowledgment...")
					go func() {
						if errAck := bot.sendAcknowledgment("newHour"); errAck != nil {
							log.Printf("❌ Failed to acknowledge 'newHour' event: %v", errAck)
						}
					}()
				}
			}

			if msgType, ok := messageData["type"].(string); ok && (msgType == "chat" || msgType == "message") {
				if data, dataOk := messageData["data"].(map[string]interface{}); dataOk {
					username, _ := data["username"].(string)
					chatMsg, _ := data["message"].(string)
					if username != "" && chatMsg != "" {
						log.Printf("💬 Chat [%s]: %s", username, chatMsg)
					}
				}
			}
			if user, userOk := messageData["user"].(string); userOk {
				if text, textOk := messageData["text"].(string); textOk {
					log.Printf("💬 Chat [%s]: %s", user, text)
				}
			}
		} else {
			log.Printf("⚠️ Could not parse message as JSON: %v. Message: %s", errJson, string(message))
		}
	}
}

// sendPeriodicMessages sends messages from the configured list at intervals
func (bot *ChatBot) sendPeriodicMessages() {
	if len(bot.messages) == 0 || bot.messages[0] == "ADD_YOUR_MESSAGES_HERE" {
		log.Println("⚠️ No valid periodic messages configured. Periodic sender will not run.")
		return
	}
	log.Printf("🕐 Starting periodic message sender (interval: %v)", bot.messageInterval)

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
			log.Printf("⏰ Time to send periodic message [%d/%d]: '%s'", bot.currentMsgIndex+1, len(bot.messages), message)
			if err := bot.sendMessage(message); err != nil {
				log.Printf("❌ Failed to send periodic message: %v", err)
			} else {
				log.Printf("✅ Periodic message sent successfully: '%s'", message)
			}
			bot.currentMsgIndex = (bot.currentMsgIndex + 1) % len(bot.messages)
		} else if bot.wsConn == nil {
			log.Println("⚠️ Skipping periodic message - WebSocket not connected.")
		}

		bot.timerMutex.Lock()
		if bot.messageTimer != nil {
			bot.messageTimer.Reset(bot.messageInterval)
		}
		bot.timerMutex.Unlock()
	}
}

// startConsoleInput reads user input from console for sending messages or commands
func (bot *ChatBot) startConsoleInput() {
	log.Println("⌨️  Starting console input reader...")
	log.Println("⌨️  Type messages and press Enter to send to chat.")
	log.Println("⌨️  Special commands: '/quit', '/help', '/status'. Other '/...' inputs are sent as messages.")

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		isHandledCommand := false

		if strings.HasPrefix(input, "/") {
			command := strings.Fields(input)[0]

			switch command {
			case "/quit", "/exit", "/q":
				log.Println("⌨️  Quit command received. Shutting down...")
				bot.Stop()
				os.Exit(0)
			case "/help", "/h":
				log.Println("⌨️  Available Bot Commands:")
				log.Println("    /quit, /exit, /q      - Stops the bot and exits.")
				log.Println("    /help, /h             - Shows this help message.")
				log.Println("    /status               - Shows the current bot status.")
				log.Println("    Any other text input (including those starting with '/')")
				log.Println("    will be sent as a chat message to the current room.")
				isHandledCommand = true
			case "/status":
				log.Printf("⌨️  Bot Status:")
				log.Printf("    - Username:          %s", bot.username)
				log.Printf("    - Current Room ID:   %s (updated by server messages)", bot.currentRoom)
				log.Printf("    - WebSocket State:   %s", map[bool]string{true: "Connected", false: "Disconnected"}[bot.wsConn != nil])
				log.Printf("    - Message Interval:  %v", bot.messageInterval)
				log.Printf("    - Periodic Messages: %d configured", len(bot.messages))
				isHandledCommand = true
			}
		}

		if !isHandledCommand {
			log.Printf("⌨️  Sending as message via console: '%s'", input)
			if err := bot.sendConsoleMessage(input); err != nil {
				log.Printf("❌ Failed to send console message: %v", err)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("❌ Error reading from console: %v", err)
	}
}


// sendKeepAlive sends WebSocket ping messages periodically
func (bot *ChatBot) sendKeepAlive() {
	log.Println("💓 Starting keep-alive sender (30 second interval)")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if bot.wsConn != nil {
			bot.wsMutex.Lock()
			if err := bot.wsConn.WriteMessage(websocket.PingMessage, []byte("keepalive")); err != nil {
				log.Printf("❌ Failed to send ping: %v", err)
			}
			bot.wsMutex.Unlock()
		}
	}
}

// Start initializes and runs the bot
func (bot *ChatBot) Start() error {
	log.Println("🚀 Starting Skyskraber chat bot...")

	log.Println("🔐 Step 1: Authenticating...")
	if err := bot.login(); err != nil {
		return fmt.Errorf("login failed: %v", err)
	}

	log.Println("🔌 Step 2: Establishing WebSocket connection...")
	if err := bot.connectWebSocket(); err != nil {
		return fmt.Errorf("WebSocket connection failed: %v", err)
	}

	log.Println("👂 Step 3: Starting message listener...")
	go bot.listenForMessages()

	log.Println("🕐 Step 4: Starting periodic message sender...")
	go bot.sendPeriodicMessages()

	log.Println("💓 Step 5: Starting keep-alive routine...")
	go bot.sendKeepAlive()

	log.Println("⌨️  Step 6: Starting console input...")
	go bot.startConsoleInput()

	log.Printf("✅ Bot started successfully! 🎉")
	log.Printf("📊 Configuration:")
	log.Printf("    - Username: %s", bot.username)
	log.Printf("    - Message interval: %v", bot.messageInterval)
	log.Printf("    - Available messages: %d", len(bot.messages))
	log.Printf("    - Initial room (may change): %s", bot.currentRoom)
	log.Printf("    - WebSocket URL: %s", bot.wsURL)
	return nil
}

// Stop gracefully shuts down the bot
func (bot *ChatBot) Stop() {
	log.Println("🛑 Stopping bot...")

	bot.timerMutex.Lock()
	if bot.messageTimer != nil {
		bot.messageTimer.Stop()
		log.Println("⏰ Periodic message timer stopped.")
	}
	bot.timerMutex.Unlock()

	if bot.wsConn != nil {
		log.Println("🔌 Closing WebSocket connection...")
		err := bot.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			log.Printf("⚠️ Error sending close message: %v", err)
		}
		bot.wsConn.Close()
		bot.wsConn = nil
		log.Println("🔌 WebSocket connection closed.")
	}
	log.Println("✅ Bot stopped.")
}

// main is the entry point of the application
func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println(strings.Repeat("=", 60))
	log.Println("      🏢 SKYSKRABER CHAT BOT v1.1 (Go Edition) 🏢")
	log.Println(strings.Repeat("=", 60))

	log.Println("📄 Loading configuration from 'config.json'...")
	config, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("❌ CRITICAL: Failed to load config: %v. Ensure 'config.json' exists or is readable.", err)
	}

	if strings.Contains(config.Username, "CHANGE_THIS") ||
		strings.Contains(config.Password, "CHANGE_THIS") ||
		(len(config.Messages) > 0 && strings.Contains(config.Messages[0], "ADD_YOUR_MESSAGES")) {
		log.Println("")
		log.Println("🚨 CONFIGURATION REQUIRED:")
		log.Println("    Please edit 'config.json' and update at least:")
		log.Println("    - username: your actual username")
		log.Println("    - password: your actual password")
		log.Println("    - messages: your custom messages array (or remove the placeholder)")
		log.Println("    Then run the bot again!")
		log.Println("")
		os.Exit(1)
	}

	if len(os.Args) > 1 {
		config.Username = os.Args[1]
		log.Printf("📝 Username overridden by command line: %s", config.Username)
	}
	if len(os.Args) > 2 {
		config.Password = os.Args[2]
		log.Printf("📝 Password overridden by command line: [HIDDEN]")
	}
	if len(os.Args) > 3 {
		if interval, errAtoi := strconv.Atoi(os.Args[3]); errAtoi == nil && interval > 0 {
			config.IntervalSeconds = interval
			log.Printf("📝 Interval overridden by command line: %d seconds", interval)
		} else {
			log.Printf("⚠️ Invalid interval '%s' from command line, using config value: %ds", os.Args[3], config.IntervalSeconds)
		}
	}

	log.Printf("📊 Final Configuration:")
	log.Printf("    - Username: %s", config.Username)
	log.Printf("    - Password: %s", strings.Repeat("*", len(config.Password)))
	log.Printf("    - Message Interval: %d seconds", config.IntervalSeconds)
	log.Printf("    - Messages Count: %d", len(config.Messages))
	log.Printf("    - Base URL: %s", config.BaseURL)
	log.Printf("    - WebSocket URL: %s", config.WebSocketURL)

	botInstance := NewChatBot(config)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("🛑 Received signal: %v. Initiating graceful shutdown...", sig)
		botInstance.Stop()
		log.Println("👋 Goodbye!")
		os.Exit(0)
	}()

	if errStart := botInstance.Start(); errStart != nil {
		log.Fatalf("❌ CRITICAL: Failed to start bot: %v", errStart)
	}

	log.Println("✅ Bot is now running! Monitoring console for input and server messages.")
	log.Println("   Type '/help' for console commands. Press Ctrl+C or type '/quit' to stop.")

	select {}
}


