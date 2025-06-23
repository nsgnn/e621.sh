package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/bubbletea"
	"github.com/charmbracelet/wish/logging"
)

// --- Configuration ---
const (
	host        = "0.0.0.0"
	apiEndpoint = "https://e621.net/posts.json"
	userAgent   = "e6tea1/v3 t.me/TankKittyCat"
)

// --- ASCII Art ---
const e621shAscii = `

            /$$$$$$   /$$$$$$    /$$                 /$$
           /$$__  $$ /$$__  $$ /$$$$                | $$
  /$$$$$$ | $$  \__/|__/  \ $$|_  $$        /$$$$$$$| $$$$$$$
 /$$__  $$| $$$$$$$   /$$$$$$/  | $$       /$$_____/| $$__  $$
| $$$$$$$$| $$__  $$ /$$____/   | $$      |  $$$$$$ | $$  \ $$
| $$_____/| $$  \ $$| $$        | $$       \____  $$| $$  | $$
|  $$$$$$$|  $$$$$$/| $$$$$$$$ /$$$$$$ /$$ /$$$$$$$/| $$  | $$
 \_______/ \______/ |________/|______/|__/|_______/ |__/  |__/

`

// --- API Structures ---
type PostResponse struct {
	Posts []Post `json:"posts"`
}
type Post struct {
	ID    int   `json:"id"`
	Pools []int `json:"pools"`
	Score struct {
		Total int `json:"total"`
	} `json:"score"`
	Rating string `json:"rating"`
	Tags   struct {
		General   []string `json:"general"`
		Species   []string `json:"species"`
		Character []string `json:"character"`
		Copyright []string `json:"copyright"`
		Artist    []string `json:"artist"`
		Invalid   []string `json:"invalid"`
		Lore      []string `json:"lore"`
		Meta      []string `json:"meta"`
	} `json:"tags"`
	File struct {
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"file"`
	Sample struct {
		URL string `json:"url"`
		Has bool   `json:"has"`
	} `json:"sample"`
}

// --- Bubble Tea Model ---
type model struct {
	cancelPreview    context.CancelFunc
	err              error
	httpClient       *http.Client
	loading          bool
	onEntranceScreen bool
	posts            []Post
	previewViewport  viewport.Model
	query            string
	quitting         bool
	searchBox        textinput.Model
	selectedButton   int // 0 for Latest, 1 for Popular
	showFullImage    bool
	spinner          spinner.Model
	statusMessage    string
	postTable        table.Model // Renamed from 'table' for clarity
	tagViewport      viewport.Model
	showTags         bool
	currentTags      string
	currentPage      int
	jumpToPostID     int
	width, height    int
}

// --- Messages ---
type postsFetchedMsg struct{ posts []Post }
type previewLoadedMsg struct{ content string }
type errorMsg struct{ err error }
type clearStatusMsg struct{}

// --- Styles & View ---

var (
	// Colors
	background = lipgloss.Color("#282a36")
	foreground = lipgloss.Color("#f8f8f2")
	highlight  = lipgloss.Color("#bd93f9")
	subtle     = lipgloss.Color("#6272a4")
	text       = lipgloss.Color("#f8f8f2")
	keyColor   = lipgloss.Color("#50fa7b")
	valueColor = lipgloss.Color("#f1fa8c")
	errorColor = lipgloss.Color("#ff5555")

	// A regular expression for parsing key:value pairs in search queries.
	filterRegex = regexp.MustCompile(`(?P<key>\S*):(?P<value>\S*)`)

	// General Styles
	appStyle = lipgloss.NewStyle().
			Foreground(foreground)
	keyStyle         = lipgloss.NewStyle().Foreground(keyColor)
	valueStyle       = lipgloss.NewStyle().Foreground(valueColor)
	defaultTextStyle = lipgloss.NewStyle().Foreground(text)
	helpStyle        = lipgloss.NewStyle().Foreground(subtle)
	spinnerStyle     = lipgloss.NewStyle().Foreground(highlight)

	// Pane & Box Styles
	paneStyle = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder(), true).
			BorderForeground(subtle).
			Padding(0, 1)
	previewPaneStyle = paneStyle.Copy().
				Align(lipgloss.Center, lipgloss.Center)
	errorBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder(), true).
			BorderForeground(errorColor).
			Foreground(errorColor).
			Padding(1, 3)
	searchBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(subtle).
			Padding(0, 1).
			Width(50)

	// Bar Styles
	topBarStyle = lipgloss.NewStyle().
			Background(background).
			Foreground(text).
			Padding(0, 1)
	statusBar = lipgloss.NewStyle().
			Background(background).
			Foreground(text).
			Padding(0, 1)

	// Menu Styles
	quitPromptStyle = lipgloss.NewStyle().
			Padding(1, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(errorColor)
	e621shStyle = lipgloss.NewStyle().Foreground(text)
	buttonStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(subtle).
			Foreground(subtle).
			Padding(0, 3).
			Margin(1, 2)
	selectedButtonStyle = buttonStyle.Copy().
				BorderForeground(highlight).
				Foreground(text)
)

// --- Table Configurations ---
var (
	fullTableColumns = []table.Column{
		{Title: "ID", Width: 7},
		{Title: "Artist", Width: 22},
		{Title: "Score", Width: 6},
	}
	miniTableColumns = []table.Column{
		{Title: "ID", Width: 7},
		{Title: "Score", Width: 6},
	}
)

// --- Commands ---

func copyToClipboardCmd(text string) tea.Cmd {
	return func() tea.Msg {
		encodedText := base64.StdEncoding.EncodeToString([]byte(text))
		fmt.Printf("\x1b]52;c;%s\x07", encodedText)
		return nil
	}
}

func clearStatusCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return clearStatusMsg{}
	})
}

func (m *model) fetchPostsCmd() tea.Cmd {
	return func() tea.Msg {
		filteredQuery := m.query

		log.Printf("Fetching posts for final query: '%s'", filteredQuery)

		req, err := http.NewRequest("GET", apiEndpoint, nil)
		if err != nil {
			log.Printf("Error creating request: %v", err)
			return errorMsg{err}
		}
		q := req.URL.Query()
		q.Add("tags", filteredQuery)
		q.Add("page", strconv.Itoa(m.currentPage))
		q.Add("limit", "75")
		req.URL.RawQuery = q.Encode()
		req.Header.Set("User-Agent", userAgent)

		resp, err := m.httpClient.Do(req)
		if err != nil {
			log.Printf("Error performing request: %v", err)
			return errorMsg{err}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			err := fmt.Errorf("API request failed with status %s: %s", resp.Status, string(body))
			log.Printf("%v", err)
			return errorMsg{err}
		}
		var postResp PostResponse
		if err := json.NewDecoder(resp.Body).Decode(&postResp); err != nil {
			log.Printf("Error decoding JSON response: %v", err)
			return errorMsg{err}
		}
		log.Printf("Successfully fetched %d posts.", len(postResp.Posts))
		return postsFetchedMsg{postResp.Posts}
	}
}

func (m *model) getDisplayURL(p Post) string {
	if m.showFullImage {
		return p.File.URL
	}
	if p.Sample.Has && p.Sample.URL != "" {
		return p.Sample.URL
	}
	return p.File.URL
}

func downloadAndRenderImage(ctx context.Context, client *http.Client, imageURL string, w, h, xOffset, yOffset int) tea.Cmd {
	return func() tea.Msg {
		if imageURL == "" {
			return previewLoadedMsg{content: "No image URL available."}
		}

		tmpfile, err := downloadToTempFile(ctx, client, imageURL)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				// Disregard. We no longer wish to display.
				return nil
			}
			log.Printf("Failed to download image: %v", err)
			return previewLoadedMsg{content: "\n\n⚠️\n\nPreview failed to load"}
		}
		defer os.Remove(tmpfile.Name())

		paddedWidth := max(w-2, 0)
		paddedHeight := max(h-2, 0)
		lOffset := xOffset + 2
		vOffset := yOffset

		placeArg := fmt.Sprintf("--place=%dx%d@%dx%d", paddedWidth, paddedHeight, lOffset, vOffset)
		allArgs := []string{"+kitten", "icat", "-z", "-5", "--align=center", "--scale-up", "--stdin=no", placeArg, tmpfile.Name()}
		cmd := exec.CommandContext(ctx, "kitty", allArgs...)

		img, err := cmd.CombinedOutput()
		if err != nil {
			if ctx.Err() == context.Canceled {
				return nil
			}
			log.Printf("Failed to run command '%s': %s", cmd.String(), string(img))
			return previewLoadedMsg{content: "\n\n⚠️\n\nPreview failed to load"}
		}

		ansiEscapedOutput := fmt.Sprintf("\x1b[s%s\x1b[u", string(img))
		return previewLoadedMsg{content: ansiEscapedOutput}
	}
}

func downloadToTempFile(ctx context.Context, client *http.Client, url string) (*os.File, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create image request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download image, status: %s", resp.Status)
	}

	tmpfile, err := os.CreateTemp("", "tmp-*.png")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	_, err = io.Copy(tmpfile, resp.Body)
	if err != nil {
		tmpfile.Close()
		os.Remove(tmpfile.Name())
		return nil, fmt.Errorf("failed to save image to temp file: %w", err)
	}
	tmpfile.Close()
	return tmpfile, nil
}

func initialModel() model {
	ti := textinput.New()
	ti.Placeholder = "Search posts by tag"
	ti.CharLimit = 256
	ti.Width = 50
	ti.Focus()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = spinnerStyle

	postTable := table.New(
		table.WithColumns(fullTableColumns),
		table.WithRows([]table.Row{}),
		table.WithFocused(true),
		table.WithWidth(38),
	)

	st := table.DefaultStyles()
	st.Header = st.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(subtle).
		BorderBottom(true).
		Bold(true).
		Foreground(text)
	st.Selected = st.Selected.
		Foreground(text).
		Background(background).
		Bold(false)
	postTable.SetStyles(st)

	vp := viewport.New(0, 0)
	tagVp := viewport.New(0, 0)

	return model{
		httpClient:       &http.Client{Timeout: 30 * time.Second},
		searchBox:        ti,
		spinner:          s,
		postTable:        postTable,
		tagViewport:      tagVp,
		previewViewport:  vp,
		showFullImage:    false,
		onEntranceScreen: true,
		selectedButton:   0, // Default to "Latest"
		quitting:         false,
		showTags:         false,
		currentTags:      "",
		currentPage:      1,
		jumpToPostID:     0,
	}
}

func (m model) Init() tea.Cmd {
	log.Println("Model Init() called.")
	initialCmd := func() tea.Msg {
		log.Println("Executing initial command: checking for kitty...")
		if _, err := exec.LookPath("kitty"); err != nil {
			log.Printf("CRITICAL: 'kitty' command not found in PATH: %v", err)
			return errorMsg{fmt.Errorf("'kitty' command not found in your PATH. Required for image previews")}
		}
		return nil
	}
	return tea.Batch(tea.ClearScreen, textinput.Blink, initialCmd)
}

func (m *model) triggerPreviewUpdate() tea.Cmd {
	if m.postTable.Cursor() >= len(m.posts) {
		return nil
	}
	selectedPost := m.posts[m.postTable.Cursor()]

	// Combine all tags into a single string
	var allTags []string
	allTags = append(allTags, selectedPost.Tags.General...)
	allTags = append(allTags, selectedPost.Tags.Species...)
	allTags = append(allTags, selectedPost.Tags.Character...)
	allTags = append(allTags, selectedPost.Tags.Copyright...)
	allTags = append(allTags, selectedPost.Tags.Artist...)
	m.currentTags = strings.Join(allTags, ", ")

	if m.cancelPreview != nil {
		m.cancelPreview()
	}
	var ctx context.Context
	ctx, m.cancelPreview = context.WithCancel(context.Background())

	m.previewViewport.SetContent(m.spinner.View() + " Loading preview...")
	topBarHeight := lipgloss.Height(m.topBarView())
	previewPaneWidth := m.width * 3 / 4
	contentHeight := m.height - topBarHeight - lipgloss.Height(m.statusBarView())
	displayURL := m.getDisplayURL(selectedPost)

	return tea.Batch(tea.ClearScreen, m.spinner.Tick, downloadAndRenderImage(ctx, m.httpClient, displayURL, previewPaneWidth, contentHeight, 0, topBarHeight))
}

// updateEntrance handles logic for the new splash screen.
func (m *model) updateEntrance(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		if m.quitting {
			switch msg.String() {
			case "y", "Y":
				return m, tea.Quit
			case "n", "N", "esc":
				m.quitting = false
				return m, nil
			}
		}

		if m.searchBox.Focused() {
			switch msg.String() {
			case "enter":
				m.query = m.searchBox.Value()
				m.currentPage = 1
				m.onEntranceScreen = false
				m.loading = true
				m.searchBox.Blur()
				cmds = append(cmds, m.fetchPostsCmd(), m.spinner.Tick)
				return m, tea.Batch(cmds...)
			case "tab":
				m.searchBox.Blur()
				return m, nil
			case "esc", "ctrl+c":
				m.quitting = true
				m.currentPage = 1
				return m, nil
			}
		} else { // Logic for when the buttons are "focused"
			switch msg.String() {
			case "enter":
				if m.selectedButton == 0 {
					m.searchBox.SetValue("")
					m.query = ""
				} else {
					m.searchBox.SetValue("order:rank")
					m.query = "order:rank"
				}
				m.onEntranceScreen = false
				m.loading = true
				cmds = append(cmds, m.fetchPostsCmd(), m.spinner.Tick)
				return m, tea.Batch(cmds...)
			case "tab":
				m.searchBox.Focus()
				return m, textinput.Blink
			case "left", "h":
				m.selectedButton = 0
				return m, nil
			case "right", "l":
				m.selectedButton = 1
				return m, nil
			case "q", "esc", "ctrl+c":
				m.quitting = true
				return m, nil
			}
		}
	}

	m.searchBox, cmd = m.searchBox.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	if m.onEntranceScreen {
		return m.updateEntrance(msg)
	}

	// If tag view is active, handle its specific keybindings and updates.
	if m.showTags {
		if keyMsg, ok := msg.(tea.KeyMsg); ok {
			switch {
			case key.Matches(keyMsg, key.NewBinding(key.WithKeys("t", "esc"))):
				m.showTags = false
				return m, nil
			}
		}

		// Pass other messages to the tag viewport for scrolling.
		m.tagViewport, cmd = m.tagViewport.Update(msg)
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width-2, msg.Height

	case postsFetchedMsg:
		m.loading = false
		m.posts = msg.posts
		m.showTags = false // Default to showing posts after a new fetch

		isMiniTable := (m.width*1/4 - 4) < 42
		if isMiniTable {
			m.postTable.SetColumns(miniTableColumns)
		} else {
			m.postTable.SetColumns(fullTableColumns)
		}

		rows := []table.Row{}
		for _, post := range m.posts {
			scoreStr := strconv.Itoa(post.Score.Total)

			if isMiniTable {
				rows = append(rows, table.Row{
					strconv.Itoa(post.ID),
					scoreStr,
				})
			} else {
				artists := strings.Join(post.Tags.Artist, ", ")
				if artists == "" {
					artists = "unknown"
				}
				rows = append(rows, table.Row{
					strconv.Itoa(post.ID),
					artists,
					scoreStr,
				})
			}
		}
		m.postTable.SetRows(rows)

		if m.jumpToPostID != 0 {
			for i, post := range m.posts {
				if post.ID == m.jumpToPostID {
					m.postTable.SetCursor(i)
					break
				}
			}
			m.jumpToPostID = 0 // Reset after use
		}

		if len(m.posts) > 0 {
			cmds = append(cmds, m.triggerPreviewUpdate())
		} else {
			m.previewViewport.SetContent("\nNo results found for your query.")
		}

	case previewLoadedMsg:
		m.previewViewport.SetContent(msg.content)
		m.previewViewport.GotoTop()

	case errorMsg:
		m.err = msg.err

	case clearStatusMsg:
		m.statusMessage = ""

	case spinner.TickMsg:
		if m.loading {
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		if m.searchBox.Focused() {
			if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
				m.query = m.searchBox.Value()
				m.loading = true
				m.searchBox.Blur()
				m.posts = []Post{}
				m.postTable.SetRows([]table.Row{})
				m.previewViewport.SetContent("")
				cmds = append(cmds, m.fetchPostsCmd(), m.spinner.Tick)
			} else if key.Matches(msg, key.NewBinding(key.WithKeys("esc"))) {
				m.searchBox.Blur()
			} else {
				m.searchBox, cmd = m.searchBox.Update(msg)
				cmds = append(cmds, cmd)
			}
		} else {
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("q", "esc"))):
				m.onEntranceScreen = true
				m.posts = []Post{}
				m.postTable.SetRows([]table.Row{})
				m.previewViewport.SetContent("")
				m.searchBox.SetValue(m.query)
				m.searchBox.Focus()
				return m, tea.Batch(textinput.Blink, tea.ClearScreen)
			case key.Matches(msg, key.NewBinding(key.WithKeys("c"))):
				if len(m.posts) > 0 && m.postTable.Cursor() < len(m.posts) {
					selectedPost := m.posts[m.postTable.Cursor()]
					m.statusMessage = "Copied link to clipboard!"
					cmds = append(cmds, copyToClipboardCmd(selectedPost.File.URL), clearStatusCmd(2*time.Second))
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("/"))):
				m.searchBox.Focus()
				cmds = append(cmds, textinput.Blink)
			case key.Matches(msg, key.NewBinding(key.WithKeys("r"))):
				m.loading = true
				m.posts = []Post{}
				m.postTable.SetRows([]table.Row{})
				m.previewViewport.SetContent("")
				cmds = append(cmds, m.fetchPostsCmd(), m.spinner.Tick, tea.ClearScreen)
			case key.Matches(msg, key.NewBinding(key.WithKeys("e"))):
				m.showFullImage = !m.showFullImage
				if len(m.posts) > 0 {
					cmds = append(cmds, m.triggerPreviewUpdate())
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("p"))):
				if !m.loading && len(m.posts) > 0 && m.postTable.Cursor() < len(m.posts) {
					selectedPost := m.posts[m.postTable.Cursor()]
					if len(selectedPost.Pools) > 0 {
						poolID := selectedPost.Pools[0]
						newQuery := fmt.Sprintf("pool:%d", poolID)
						if m.query != newQuery {
							m.jumpToPostID = selectedPost.ID
							m.query = newQuery
							m.searchBox.SetValue(m.query)
							m.currentPage = 1
							m.loading = true
							m.posts = []Post{}
							m.postTable.SetRows([]table.Row{})
							m.previewViewport.SetContent("")
							cmds = append(cmds, m.fetchPostsCmd(), m.spinner.Tick, tea.ClearScreen)
						}
					}
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("t"))):
				if len(m.posts) > 0 {
					m.showTags = !m.showTags
				}
			case key.Matches(msg, key.NewBinding(key.WithKeys("h", "left"))):
				if m.currentPage > 1 && !m.loading {
					m.currentPage = m.currentPage - 1
				}
				m.loading = true
				m.posts = []Post{}
				m.postTable.SetRows([]table.Row{})
				m.previewViewport.SetContent("")
				cmds = append(cmds, m.fetchPostsCmd(), m.spinner.Tick, tea.ClearScreen)
			case key.Matches(msg, key.NewBinding(key.WithKeys("l", "right"))):
				if m.currentPage < 750 && !m.loading {
					m.currentPage = m.currentPage + 1
				}
				m.loading = true
				m.posts = []Post{}
				m.postTable.SetRows([]table.Row{})
				m.previewViewport.SetContent("")
				cmds = append(cmds, m.fetchPostsCmd(), m.spinner.Tick, tea.ClearScreen)
			default:
				if !m.loading {
					originalCursor := m.postTable.Cursor()
					m.postTable, cmd = m.postTable.Update(msg)
					cmds = append(cmds, cmd)
					if originalCursor != m.postTable.Cursor() && len(m.posts) > 0 {
						cmds = append(cmds, m.triggerPreviewUpdate())
					}
				}
			}
		}
	}
	m.previewViewport, cmd = m.previewViewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

func (m *model) menuView() string {
	var view string

	if m.quitting {
		view = quitPromptStyle.Render("Are you sure you want to quit? (y/n)")
	} else {
		asciiArt := e621shStyle.Render(e621shAscii)

		currentSearchBoxStyle := searchBoxStyle
		if m.searchBox.Focused() {
			currentSearchBoxStyle = searchBoxStyle.Copy().BorderForeground(highlight)
		}
		searchBoxView := currentSearchBoxStyle.Render(m.searchBox.View())

		latestButton := buttonStyle.Render("Latest")
		popularButton := buttonStyle.Render("Popular")
		if !m.searchBox.Focused() {
			if m.selectedButton == 0 {
				latestButton = selectedButtonStyle.Render("Latest")
			} else {
				popularButton = selectedButtonStyle.Render("Popular")
			}
		}
		buttonsView := lipgloss.JoinHorizontal(lipgloss.Top, latestButton, "  ", popularButton)

		var helpTextContent string
		if m.searchBox.Focused() {
			helpTextContent = "enter: search | tab: select buttons | esc: quit"
		} else {
			helpTextContent = "←/→: nav | enter: select | tab: edit search | esc: quit"
		}
		helpView := helpStyle.Render(helpTextContent)

		view = lipgloss.JoinVertical(
			lipgloss.Center,
			asciiArt,
			searchBoxView,
			buttonsView,
			helpView,
		)
	}

	return lipgloss.Place(
		m.width,
		m.height,
		lipgloss.Center,
		lipgloss.Center,
		view,
	)
}

func (m *model) topBarView() string {
	spacer := "\n\n"
	topBarText := fmt.Sprintf("Query: %s", m.query)
	return spacer + topBarStyle.Width(m.width).Render(topBarText)
}

func (m *model) styledQueryText() string {
	val := m.searchBox.Value()
	cursorPos := m.searchBox.Position()
	cursorStyle := m.searchBox.Cursor.Style
	matches := filterRegex.FindAllStringSubmatchIndex(val, -1)
	styleMap := make(map[int]lipgloss.Style)
	keyIdx := filterRegex.SubexpIndex("key")
	valueIdx := filterRegex.SubexpIndex("value")

	for _, match := range matches {
		if keyIdx != -1 {
			for i := match[2*keyIdx]; i < match[2*keyIdx+1]; i++ {
				styleMap[i] = keyStyle
			}
		}
		if valueIdx != -1 {
			for i := match[2*valueIdx]; i < match[2*valueIdx+1]; i++ {
				styleMap[i] = valueStyle
			}
		}
	}

	var styledParts []string
	currentByte := 0
	for _, r := range val {
		char := string(r)
		style, ok := styleMap[currentByte]
		if !ok {
			style = defaultTextStyle
		}

		if m.searchBox.Cursor.Blink && currentByte == cursorPos {
			char = m.searchBox.Cursor.View()
		} else if currentByte == cursorPos {
			char = cursorStyle.Render(char)
		}
		styledParts = append(styledParts, style.Render(char))
		currentByte += len(char)
	}

	if cursorPos == len(val) && m.searchBox.Cursor.Blink {
		styledParts = append(styledParts, m.searchBox.Cursor.View())
	} else if cursorPos == len(val) {
		styledParts = append(styledParts, cursorStyle.Render(" "))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, styledParts...)
}

func (m *model) statusBarView() string {
	var statusText string
	if m.showTags {
		statusText = "t/esc: close tags popup"
	} else if m.searchBox.Focused() {
		statusText = "Filter: " + m.styledQueryText()
	} else if m.statusMessage != "" {
		statusText = m.statusMessage
	} else {
		imageModeText := "full/[sample]"
		if m.showFullImage {
			imageModeText = "[full]/sample"
		}
		statusText = fmt.Sprintf("↑/↓: nav | c: copy url | /: filter | r: refresh | e: %s | t: show tags popup", imageModeText)

		if !m.loading && len(m.posts) > 0 && m.postTable.Cursor() < len(m.posts) {
			selectedPost := m.posts[m.postTable.Cursor()]
			if len(selectedPost.Pools) > 0 {
				statusText += " | p: view pool"
			}
		}

		statusText += " | esc: back to menu"
	}
	statusBar.Width(m.width)
	return statusBar.Render(statusText)
}

func (m model) View() string {
	if m.width == 0 {
		return "Initializing..."
	}

	var finalView string
	if m.onEntranceScreen {
		finalView = m.menuView()
	} else if m.err != nil {
		errText := fmt.Sprintf("An error occurred:\n\n%s\n\nPress Esc to quit.", m.err.Error())
		ui := lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, errorBoxStyle.Render(errText))
		finalView = ui
	} else if m.loading {
		text := fmt.Sprintf("\n   %s Fetching data for '%s'...\n", m.spinner.View(), m.query)
		ui := lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, text)
		finalView = ui
	} else {
		topBarView := m.topBarView()
		statusBarView := m.statusBarView()
		contentHeight := m.height - lipgloss.Height(topBarView) - lipgloss.Height(statusBarView)

		previewPaneWidth := m.width * 3 / 4
		sidePaneWidth := m.width - previewPaneWidth - 4

		m.previewViewport.Width = previewPaneWidth - 2
		m.previewViewport.Height = contentHeight - 2

		m.postTable.SetWidth(sidePaneWidth - 2)
		m.postTable.SetHeight(contentHeight - 2)
		m.tagViewport.Width = sidePaneWidth - 2
		m.tagViewport.Height = contentHeight - 2

		previewPane := previewPaneStyle.
			Width(previewPaneWidth).
			Height(contentHeight).
			Render(m.previewViewport.View())

		var sidePaneView string
		if m.showTags {
			wrappedContent := lipgloss.NewStyle().
				Width(m.tagViewport.Width).
				Render("Tags:\n\n" + m.currentTags)
			m.tagViewport.SetContent(wrappedContent)
			sidePaneView = paneStyle.
				Width(sidePaneWidth).
				Height(contentHeight).
				Render(m.tagViewport.View())
		} else {
			sidePaneView = paneStyle.
				Width(sidePaneWidth).
				Height(contentHeight).
				Render(m.postTable.View())
		}

		mainView := lipgloss.JoinHorizontal(lipgloss.Top, previewPane, sidePaneView)
		finalView = lipgloss.JoinVertical(lipgloss.Left, topBarView, mainView, statusBarView)
	}

	return appStyle.Render(finalView)
}

// --- Main SSH Server ---

func setupLogger() (*os.File, error) {
	logFilePath := os.Getenv("E6TEA_LOG_FILE")
	if logFilePath == "" {
		logFilePath = "e6tea.log" // Default log file path
	}
	f, err := os.OpenFile(logFilePath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}
	log.SetOutput(f)
	return f, nil
}

func main() {
	logFile, err := setupLogger()
	if err != nil {
		log.Fatalf("could not set up logger: %v", err)
	}
	defer logFile.Close()

	log.Println("--------------------")
	log.Println("Logger initialized. Starting server...")

	port := os.Getenv("E6TEA_PORT")
	if port == "" {
		port = "2222" // Default port
	}

	s, err := wish.NewServer(
		wish.WithAddress(fmt.Sprintf("%s:%s", host, port)),
		wish.WithHostKeyPath(".ssh/term_info_ed25519"),
		wish.WithMiddleware(
			bubbletea.Middleware(teaHandler),
			logging.Middleware(),
		),
	)
	if err != nil {
		log.Fatalf("could not start server: %v", err)
	}

	done := make(chan os.Signal, 1)
	log.Printf("Starting SSH server on %s:%s", host, port)
	go func() {
		if err = s.ListenAndServe(); err != nil && err != ssh.ErrServerClosed {
			log.Fatalf("server exited with error: %v", err)
		}
		done <- nil
	}()

	<-done
	log.Println("Stopping SSH server.")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()
	if err := s.Shutdown(ctx); err != nil && err != ssh.ErrServerClosed {
		log.Fatalf("could not stop server gracefully: %v", err)
	}
}

func teaHandler(s ssh.Session) (tea.Model, []tea.ProgramOption) {
	pty, _, active := s.Pty()
	if !active {
		wish.Fatalln(s, "no active PTY found")
		return nil, nil
	}
	m := initialModel()
	m.width = pty.Window.Width
	m.height = pty.Window.Height
	return m, []tea.ProgramOption{tea.WithInput(s), tea.WithOutput(s), tea.WithAltScreen()}
}
