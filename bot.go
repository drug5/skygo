package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// NewChatBot creates a new ChatBot instance
func NewChatBot(config *Config, tui *TUI) *ChatBot {
	return &ChatBot{
		username:        config.Username,
		password:        config.Password,
		baseURL:         config.BaseURL,
		wsURL:           config.WebSocketURL,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		intervalConfig:  config.IntervalSeconds,
		currentRoom:     "1",
		currentRoomName: "Unknown",
		messages:        config.Messages,
		currentMsgIndex: 0,
		tui:             tui,
		isConnected:     false,
		reconnectCount:  0,
		// Initialize autopick with client ID tracking
		autopickEnabled:    false,
		clientIDToUsername: make(map[int]string),
		clientPositions:    make(map[int]Position),
		droppedItems:       make(map[int]DroppedItem),
		ownClientID:        0,
		ownPosition:        Position{X: 0, Y: 0},
		// Initialize room fields
		roomFields:      make([]RoomField, 0),
		// Initialize login action
		loginAction:     config.LoginAction,
		loginActionDone: false,
	}
}

// log is a helper function to log to the TUI instead of stdout
func (bot *ChatBot) log(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	if bot.tui != nil {
		bot.tui.AddLogMessage(message)
	}
}

// getRandomInterval calculates a random interval between configured from/to values
func (bot *ChatBot) getRandomInterval() time.Duration {
	if bot.intervalConfig == nil {
		return 300 * time.Second // Default fallback
	}
	
	from := bot.intervalConfig.From
	to := bot.intervalConfig.To
	
	if from == to {
		return time.Duration(from) * time.Second
	}
	
	// Generate random number between from and to (inclusive)
	randomSeconds := from + rand.Intn(to-from+1)
	return time.Duration(randomSeconds) * time.Second
}

// getIntervalString returns a human-readable string describing the interval configuration
func (bot *ChatBot) getIntervalString() string {
	if bot.intervalConfig == nil {
		return "300s (default)"
	}
	
	if bot.intervalConfig.From == bot.intervalConfig.To {
		return fmt.Sprintf("%ds (fixed)", bot.intervalConfig.From)
	}
	
	return fmt.Sprintf("%d-%ds (random)", bot.intervalConfig.From, bot.intervalConfig.To)
}

// setConnectionStatus safely updates connection status
func (bot *ChatBot) setConnectionStatus(connected bool) {
	bot.connMutex.Lock()
	defer bot.connMutex.Unlock()

	wasConnected := bot.isConnected
	bot.isConnected = connected

	if connected && !wasConnected {
		bot.reconnectCount = 0
		bot.updateFullStatus()
		bot.log("🟢 Connection status: CONNECTED")
		
		// Execute login actions if this is a new connection
		go bot.executeLoginActions()
	} else if !connected && wasConnected {
		bot.updateFullStatus()
		bot.log("🔴 Connection status: DISCONNECTED")
	}
}

// updateFullStatus updates the complete status bar
func (bot *ChatBot) updateFullStatus() {
	autopickStatus := "OFF"
	if bot.isAutopickEnabled() {
		autopickStatus = "ON"
	}
	bot.tui.UpdateStatusBar(bot.username, bot.isConnected, bot.currentRoomName, bot.currentRoom, autopickStatus)
}

// isConnectionHealthy checks if connection is healthy
func (bot *ChatBot) isConnectionHealthy() bool {
	bot.connMutex.RLock()
	defer bot.connMutex.RUnlock()
	return bot.isConnected && bot.wsConn != nil
}

// calculateReconnectDelay returns exponential backoff delay
func (bot *ChatBot) calculateReconnectDelay() time.Duration {
	baseDelay := 5 * time.Second
	maxDelay := 5 * time.Minute

	delay := time.Duration(baseDelay.Nanoseconds() * int64(1<<min(bot.reconnectCount, 6)))
	if delay > maxDelay {
		delay = maxDelay
	}

	return delay
}

// min helper function
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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

// fullReconnect performs complete reconnection including re-authentication
func (bot *ChatBot) fullReconnect() error {
	bot.reconnectCount++
	delay := bot.calculateReconnectDelay()

	bot.log("🔄 Attempting full reconnection #%d (delay: %v)...", bot.reconnectCount, delay)
	autopickStatus := "OFF"
	if bot.isAutopickEnabled() {
		autopickStatus = "ON"
	}
	bot.tui.UpdateStatusBar(bot.username, false, bot.currentRoomName, fmt.Sprintf("Reconnecting... (attempt %d)", bot.reconnectCount), autopickStatus)

	// Wait with exponential backoff
	time.Sleep(delay)

	// Close existing connection if any
	if bot.wsConn != nil {
		bot.wsConn.Close()
		bot.wsConn = nil
	}

	bot.setConnectionStatus(false)

	// Step 1: Re-authenticate if enough time has passed or if we've had many failures
	timeSinceLastReconnect := time.Since(bot.lastReconnect)
	if timeSinceLastReconnect > 10*time.Minute || bot.reconnectCount > 3 {
		bot.log("🔐 Re-authenticating due to extended disconnection...")
		if err := bot.login(); err != nil {
			bot.log("❌ Re-authentication failed: %v", err)
			return fmt.Errorf("re-authentication failed: %v", err)
		}
		bot.log("✅ Re-authentication successful")
	}

	// Step 2: Establish WebSocket connection
	bot.log("🔌 Re-establishing WebSocket connection...")
	if err := bot.connectWebSocket(); err != nil {
		bot.log("❌ WebSocket reconnection failed: %v", err)
		return fmt.Errorf("WebSocket reconnection failed: %v", err)
	}

	bot.setConnectionStatus(true)
	bot.lastReconnect = time.Now()
	bot.log("✅ Full reconnection successful!")
	bot.tui.AddChatMessage("", "", "*** Reconnected to chat server ***")

	return nil
}

// Start initializes and runs the bot
func (bot *ChatBot) Start() error {
	bot.log("🚀 Starting Skyskraber chat bot...")
	bot.tui.UpdateStatusBar(bot.username, false, "", "Starting...", "OFF")

	bot.log("🔐 Step 1: Authenticating...")
	bot.tui.UpdateStatusBar(bot.username, false, "", "Authenticating...", "OFF")
	if err := bot.login(); err != nil {
		return fmt.Errorf("login failed: %v", err)
	}

	bot.log("🔌 Step 2: Establishing WebSocket connection...")
	bot.tui.UpdateStatusBar(bot.username, false, "", "Connecting...", "OFF")
	if err := bot.connectWebSocket(); err != nil {
		return fmt.Errorf("WebSocket connection failed: %v", err)
	}

	// Mark as connected after successful connection
	bot.setConnectionStatus(true)
	bot.lastReconnect = time.Now()

	bot.log("👂 Step 3: Starting message listener...")
	go bot.listenForMessages()

	bot.log("🕐 Step 4: Starting periodic message sender...")
	go bot.sendPeriodicMessages()

	bot.log("💓 Step 5: Starting keep-alive routine...")
	go bot.sendKeepAlive()

	bot.log("⏰ Step 6: Starting status updater...")
	go bot.startStatusUpdater()

	bot.log("⌨️ Step 7: Setting up input handler...")
	bot.setupInputHandler()

	bot.log("✅ Bot started successfully! 🎉")
	bot.updateFullStatus()

	bot.tui.AddChatMessage("", "", "=== Skyskraber Chat Bot Started ===")
	bot.tui.AddChatMessage("", "", "Type /help for available commands")
	bot.tui.AddChatMessage("", "", "Press Ctrl+C or type /quit to exit")
	bot.tui.AddChatMessage("", "", "📁 Logs are being saved to logs/ directory")
	bot.tui.AddChatMessage("", "", "🤖 Autopick is DISABLED by default - use /autopick to enable")
	bot.tui.AddChatMessage("", "", "🎲 Use /move to move to random open field")
	bot.tui.AddChatMessage("", "", "⬆️⬇️ Use arrow keys to navigate input history")

	// Show login action status
	if bot.loginAction != nil && bot.loginAction.LoginActionEnabled {
		bot.tui.AddChatMessage("", "", fmt.Sprintf("🤖 Login actions ENABLED - %d commands will execute in %d seconds", len(bot.loginAction.Commands), bot.loginAction.WaitSeconds))
	}

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

	bot.log("📁 Closing log files...")
	bot.tui.closeLogFiles()
	bot.log("✅ Bot stopped.")
}

// sendKeepAlive sends WebSocket ping messages periodically
func (bot *ChatBot) sendKeepAlive() {
	bot.log("💓 Starting keep-alive sender (30 second interval)")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if bot.isConnectionHealthy() {
			bot.wsMutex.Lock()
			bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := bot.wsConn.WriteMessage(websocket.PingMessage, []byte("keepalive")); err != nil {
				bot.log("❌ Failed to send ping: %v", err)
				// Mark connection as unhealthy on ping failure
				bot.setConnectionStatus(false)
			} else {
				bot.log("💓 Keep-alive ping sent successfully")
			}
			bot.wsMutex.Unlock()
		} else {
			bot.log("⚠️ Skipping keep-alive ping - connection not healthy")
		}
	}
}

// startStatusUpdater keeps the status bar current time updated
func (bot *ChatBot) startStatusUpdater() {
	bot.log("⏰ Starting status updater (5 second interval)")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Update the status bar with current time (other info stays the same)
		bot.updateFullStatus()
	}
}
