package vm

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bubunyo/monolith"
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	sparkCap     = 120 // max sparkline history (2 minutes at 1s tick)
	cpuSmoothing = 3   // number of ticks to average CPU over
	tickInterval = 1 * time.Second
)

// Sparkline characters: 8 levels from low to high.
var sparkChars = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// ── Ring buffer for sparkline data ──────────────────────────────────────────

type ringBuffer struct {
	data []float64
	head int // next write position
	len  int
	cap  int
}

func newRingBuffer(cap int) ringBuffer {
	return ringBuffer{
		data: make([]float64, cap),
		cap:  cap,
	}
}

func (r *ringBuffer) push(v float64) {
	r.data[r.head] = v
	r.head = (r.head + 1) % r.cap
	if r.len < r.cap {
		r.len++
	}
}

// last returns the most recent n values in chronological order.
func (r *ringBuffer) last(n int) []float64 {
	if n > r.len {
		n = r.len
	}
	if n == 0 {
		return nil
	}
	out := make([]float64, n)
	start := (r.head - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		out[i] = r.data[(start+i)%r.cap]
	}
	return out
}

// ── Sparkline renderer ──────────────────────────────────────────────────────

// renderSparkline renders a sparkline string of the given width.
// fixedMax controls scaling: if > 0, values are scaled to [0, fixedMax].
// Otherwise auto-scales to the peak in the visible window.
func renderSparkline(buf *ringBuffer, width int, fixedMax float64) string {
	vals := buf.last(width)
	if len(vals) == 0 {
		return strings.Repeat(string(sparkChars[0]), width)
	}

	maxVal := fixedMax
	if maxVal <= 0 {
		// Auto-scale to peak in visible window.
		for _, v := range vals {
			if v > maxVal {
				maxVal = v
			}
		}
		if maxVal <= 0 {
			maxVal = 1
		}
	}

	var sb strings.Builder
	for _, v := range vals {
		idx := int(v / maxVal * float64(len(sparkChars)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparkChars) {
			idx = len(sparkChars) - 1
		}
		sb.WriteRune(sparkChars[idx])
	}
	// Pad with baseline if we have fewer values than width.
	for i := len(vals); i < width; i++ {
		sb.WriteRune(sparkChars[0])
	}
	return sb.String()
}

// ── Bubbletea model ─────────────────────────────────────────────────────────

type tickMsg time.Time

type model struct {
	name   string
	cfg    monolith.VMConfig
	state  *monolith.VMState
	vmenet string // cached vmenet interface name

	// Current tick values
	cpuPct     float64
	memRSS     int64
	diskActual int64
	diskTotal  int64
	rxRate     float64 // bytes/sec
	txRate     float64 // bytes/sec
	rxTotal    int64
	txTotal    int64
	cpuTime    string
	ip         string
	uptime     time.Duration

	// Previous tick for rate calculation
	prevRx   int64
	prevTx   int64
	prevTick time.Time
	hasPrev  bool

	// CPU smoothing
	cpuWindow [cpuSmoothing]float64
	cpuIdx    int
	cpuCount  int

	// Sparkline histories
	cpuSpark ringBuffer
	memSpark ringBuffer
	rxSpark  ringBuffer
	txSpark  ringBuffer

	// UI components
	cpuBar  progress.Model
	memBar  progress.Model
	diskBar progress.Model

	width    int
	height   int
	quitting bool
	stopped  bool
}

func newModel(name string) model {
	cfg, _ := monolith.ReadConfig(name)

	cpuBar := progress.New(
		progress.WithSolidFill("255"),
		progress.WithoutPercentage(),
	)
	memBar := progress.New(
		progress.WithSolidFill("255"),
		progress.WithoutPercentage(),
	)
	diskBar := progress.New(
		progress.WithSolidFill("255"),
		progress.WithoutPercentage(),
	)

	return model{
		name:     name,
		cfg:      cfg,
		cpuBar:   cpuBar,
		memBar:   memBar,
		diskBar:  diskBar,
		cpuSpark: newRingBuffer(sparkCap),
		memSpark: newRingBuffer(sparkCap),
		rxSpark:  newRingBuffer(sparkCap),
		txSpark:  newRingBuffer(sparkCap),
		width:    80,
		height:   24,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		m.gatherStats()
		return m, tea.Tick(tickInterval, func(t time.Time) tea.Msg {
			return tickMsg(t)
		})
	}

	return m, nil
}

func (m *model) gatherStats() {
	// Re-read raw state to check VM is still running.
	state, err := monolith.ReadRawState(m.name)
	if err != nil {
		m.stopped = true
		return
	}
	m.state = state

	// Process stats.
	cpuRaw, rssBytes, cpuTime := monolith.ProcessStatsEx(state.PID)
	m.memRSS = rssBytes
	m.cpuTime = cpuTime

	// CPU smoothing: 3-tick moving average.
	m.cpuWindow[m.cpuIdx%cpuSmoothing] = cpuRaw
	m.cpuIdx++
	if m.cpuCount < cpuSmoothing {
		m.cpuCount++
	}
	var cpuSum float64
	for i := 0; i < m.cpuCount; i++ {
		cpuSum += m.cpuWindow[i]
	}
	m.cpuPct = cpuSum / float64(m.cpuCount)

	// Sparklines.
	m.cpuSpark.push(m.cpuPct)
	if m.cfg.Memory > 0 {
		m.memSpark.push(float64(m.memRSS) / float64(m.cfg.Memory) * 100)
	}

	// IP and uptime.
	m.ip = lookupIPForTop(m.cfg.Network, state.MAC)
	if state.StartedAt > 0 {
		m.uptime = time.Since(time.Unix(state.StartedAt, 0))
	}

	// Disk stats.
	dir, err := vmsDir()
	if err == nil {
		diskPath := filepath.Join(dir, m.name, "disk.img")
		if info, err := os.Stat(diskPath); err == nil {
			m.diskTotal = info.Size()
		}
		var st syscall.Stat_t
		if err := syscall.Stat(diskPath, &st); err == nil {
			m.diskActual = st.Blocks * 512
		}
	}

	// Network stats.
	// For "none" mode there's no network device.
	// For "host" mode the VM uses a bridged physical NIC; no per-VM vmenet exists.
	hasVmenet := m.cfg.Network != monolith.NetworkNone && m.cfg.Network != monolith.NetworkHost
	if hasVmenet {
		if m.vmenet == "" {
			m.vmenet = monolith.VmenetForMAC(state.MAC)
		}
		if m.vmenet != "" {
			ns := monolith.NetworkStats(m.vmenet)
			now := time.Now()
			if m.hasPrev {
				dt := now.Sub(m.prevTick).Seconds()
				if dt > 0 {
					m.rxRate = float64(ns.RxBytes-m.prevRx) / dt
					m.txRate = float64(ns.TxBytes-m.prevTx) / dt
					if m.rxRate < 0 {
						m.rxRate = 0
					}
					if m.txRate < 0 {
						m.txRate = 0
					}
				}
			}
			m.prevRx = ns.RxBytes
			m.prevTx = ns.TxBytes
			m.prevTick = now
			m.hasPrev = true
			m.rxTotal = ns.RxBytes
			m.txTotal = ns.TxBytes

			m.rxSpark.push(m.rxRate)
			m.txSpark.push(m.txRate)
		}
	}
}

// ── View ────────────────────────────────────────────────────────────────────

func (m model) View() string {
	if m.quitting {
		return ""
	}
	if m.stopped {
		return "\n  VM has stopped.\n\n  Press q to exit.\n"
	}

	contentWidth := m.width - 2 // leave margin
	if contentWidth < 40 {
		contentWidth = 40
	}
	if contentWidth > 80 {
		contentWidth = 80
	}
	innerWidth := contentWidth - 4 // borders + padding

	// Styles — monotone palette (whites and grays)
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1).
		Width(contentWidth)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("255"))

	labelStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("245"))

	valueStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("255"))

	sparkStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("250"))

	sparkStyleTx := lipgloss.NewStyle().
		Foreground(lipgloss.Color("246"))

	dimStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241"))

	// Bar widths
	barWidth := innerWidth - 8 // space for percentage text
	if barWidth < 10 {
		barWidth = 10
	}
	m.cpuBar.Width = barWidth
	m.memBar.Width = barWidth
	m.diskBar.Width = barWidth

	sparkWidth := innerWidth
	if sparkWidth < 10 {
		sparkWidth = 10
	}

	// ── Header box ──
	uptimeStr := "-"
	if m.uptime > 0 {
		uptimeStr = formatUptime(m.uptime)
	}
	ipStr := m.ip
	if ipStr == "" {
		ipStr = "-"
	}

	header := boxStyle.Render(
		titleStyle.Render(fmt.Sprintf("  %s", m.name)) + "\n" +
			fmt.Sprintf("  %s %s    %s %s    %s %s",
				labelStyle.Render("State:"), valueStyle.Render("running"),
				labelStyle.Render("IP:"), valueStyle.Render(ipStr),
				labelStyle.Render("Uptime:"), valueStyle.Render(uptimeStr),
			) + "\n" +
			fmt.Sprintf("  %s %s           %s %s      %s %s",
				labelStyle.Render("CPUs:"), valueStyle.Render(fmt.Sprintf("%d", m.cfg.CPUs)),
				labelStyle.Render("Memory:"), valueStyle.Render(formatSize(int64(m.cfg.Memory))),
				labelStyle.Render("OS:"), valueStyle.Render("Alpine 3.21"),
			),
	)

	// ── CPU box ──
	cpuPercent := m.cpuPct / 100.0
	if cpuPercent > 1 {
		cpuPercent = 1
	}
	cpuTimeStr := m.cpuTime
	if cpuTimeStr == "" {
		cpuTimeStr = "0:00.00"
	}

	cpuBox := boxStyle.Render(
		titleStyle.Render("  CPU") + "\n" +
			fmt.Sprintf("  %s %s",
				m.cpuBar.ViewAs(cpuPercent),
				valueStyle.Render(fmt.Sprintf("%5.1f%%", m.cpuPct)),
			) + "\n" +
			fmt.Sprintf("  %s %s",
				labelStyle.Render("Time:"),
				valueStyle.Render(cpuTimeStr),
			) + "\n" +
			"  " + sparkStyle.Render(renderSparkline(&m.cpuSpark, sparkWidth, 100)),
	)

	// ── Memory box ──
	memPercent := 0.0
	if m.cfg.Memory > 0 {
		memPercent = float64(m.memRSS) / float64(m.cfg.Memory)
	}
	if memPercent > 1 {
		memPercent = 1
	}

	memBox := boxStyle.Render(
		titleStyle.Render("  Memory") + "\n" +
			fmt.Sprintf("  %s %s",
				m.memBar.ViewAs(memPercent),
				valueStyle.Render(fmt.Sprintf("%5.1f%%", memPercent*100)),
			) + "\n" +
			fmt.Sprintf("  %s / %s",
				valueStyle.Render(formatSize(m.memRSS)),
				labelStyle.Render(formatSize(int64(m.cfg.Memory))),
			) + "\n" +
			"  " + sparkStyle.Render(renderSparkline(&m.memSpark, sparkWidth, 100)),
	)

	// ── Network box ──
	var netBox string
	switch m.cfg.Network {
	case monolith.NetworkNone:
		netBox = boxStyle.Render(
			titleStyle.Render("  Network") + "\n" +
				"  " + dimStyle.Render("No network (--network none)"),
		)
	case monolith.NetworkHost:
		netBox = boxStyle.Render(
			titleStyle.Render("  Network") + "\n" +
				fmt.Sprintf("  %s %s  %s %s",
					labelStyle.Render("Mode:"), valueStyle.Render("host"),
					labelStyle.Render("Interface:"), valueStyle.Render(m.cfg.HostInterface),
				) + "\n" +
				"  " + dimStyle.Render("Per-VM stats not available in host mode"),
		)
	default:
		netBox = boxStyle.Render(
			titleStyle.Render("  Network") + "\n" +
				fmt.Sprintf("  %s %s  %s    %s %s  %s",
					labelStyle.Render("RX:"),
					valueStyle.Render(formatRate(m.rxRate)),
					dimStyle.Render(fmt.Sprintf("(%s total)", formatBytes(m.rxTotal))),
					labelStyle.Render("TX:"),
					valueStyle.Render(formatRate(m.txRate)),
					dimStyle.Render(fmt.Sprintf("(%s total)", formatBytes(m.txTotal))),
				) + "\n" +
				fmt.Sprintf("  %s %s",
					labelStyle.Render("RX"),
					sparkStyle.Render(renderSparkline(&m.rxSpark, sparkWidth-3, 0)),
				) + "\n" +
				fmt.Sprintf("  %s %s",
					labelStyle.Render("TX"),
					sparkStyleTx.Render(renderSparkline(&m.txSpark, sparkWidth-3, 0)),
				),
		)
	}

	// ── Disk box ──
	diskPercent := 0.0
	if m.diskTotal > 0 {
		diskPercent = float64(m.diskActual) / float64(m.diskTotal)
	}
	if diskPercent > 1 {
		diskPercent = 1
	}

	diskBox := boxStyle.Render(
		titleStyle.Render("  Disk") + "\n" +
			fmt.Sprintf("  %s %s",
				m.diskBar.ViewAs(diskPercent),
				valueStyle.Render(fmt.Sprintf("%5.1f%%", diskPercent*100)),
			) + "\n" +
			fmt.Sprintf("  %s / %s",
				valueStyle.Render(formatSize(m.diskActual)),
				labelStyle.Render(formatSize(m.diskTotal)),
			),
	)

	// ── Footer ──
	footer := dimStyle.Render("  q: quit")

	return "\n" + header + "\n\n" + cpuBox + "\n\n" + memBox + "\n\n" + netBox + "\n\n" + diskBox + "\n\n" + footer + "\n"
}

// ── Formatting helpers ──────────────────────────────────────────────────────

func formatBytes(b int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func formatRate(bytesPerSec float64) string {
	b := int64(math.Round(bytesPerSec))
	return formatBytes(b) + "/s"
}

// lookupIPForTop picks the right IP discovery strategy for the top view.
func lookupIPForTop(network, mac string) string {
	switch network {
	case monolith.NetworkNone, monolith.NetworkIsolated:
		return ""
	default:
		return monolith.LookupIPByMAC(mac)
	}
}
