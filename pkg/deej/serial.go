package deej

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobsa/go-serial/serial"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

const (
	reconnectMinBackoff = 1 * time.Second
	reconnectMaxBackoff = 30 * time.Second
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	deej   *Deej
	logger *zap.SugaredLogger

	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	connected bool
	conn      io.ReadWriteCloser

	connOptions serial.OpenOptions

	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent

	// nudged when Windows resumes from sleep/hibernate (optional)
	resumeSignal chan struct{}
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

var expectedLinePattern = regexp.MustCompile(`^\d{1,4}(\|\d{1,4})*\r\n$`)

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                deej,
		logger:              logger,
		sliderMoveConsumers: []chan SliderMoveEvent{},
		resumeSignal:        make(chan struct{}, 1),
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()
	sio.setupResumeListener()

	return sio, nil
}

// Start begins maintaining a serial connection, reconnecting automatically
// after disconnects (including sleep/hibernate wake).
func (sio *SerialIO) Start() error {
	sio.mu.Lock()
	defer sio.mu.Unlock()

	if sio.cancel != nil {
		sio.logger.Warn("Serial manager already started")
		return errors.New("serial: already started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	sio.cancel = cancel
	sio.done = make(chan struct{})

	go func() {
		defer close(sio.done)
		sio.maintainConnection(ctx)
	}()

	return nil
}

// Stop shuts down the serial manager and closes any active connection.
func (sio *SerialIO) Stop() {
	sio.mu.Lock()
	cancel := sio.cancel
	done := sio.done
	sio.cancel = nil
	sio.mu.Unlock()

	if cancel == nil {
		sio.logger.Debug("Serial manager not running, nothing to stop")
		return
	}

	sio.logger.Debug("Shutting down serial manager")
	cancel()

	if done != nil {
		<-done
	}
}

// SubscribeToSliderMoveEvents returns a buffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent, 32)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)
	return ch
}

// NotifyResume hints that the machine may have woken from sleep/hibernate
// so a reconnect should be attempted promptly.
func (sio *SerialIO) NotifyResume() {
	select {
	case sio.resumeSignal <- struct{}{}:
	default:
	}
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for range configReloadedChannel {
			// unset slider count so the next line re-applies volumes after session remap
			go func() {
				<-time.After(stopDelay)
				sio.mu.Lock()
				sio.lastKnownNumSliders = 0
				sio.mu.Unlock()
			}()

			sio.mu.Lock()
			portChanged := sio.connOptions.PortName != "" &&
				(sio.deej.config.ConnectionInfo.COMPort != sio.connOptions.PortName ||
					uint(sio.deej.config.ConnectionInfo.BaudRate) != sio.connOptions.BaudRate)
			running := sio.cancel != nil
			sio.mu.Unlock()

			if !portChanged {
				continue
			}

			sio.logger.Info("Detected change in connection parameters, attempting to renew connection")
			if running {
				sio.Stop()
				<-time.After(stopDelay)
			}

			if err := sio.Start(); err != nil {
				sio.logger.Warnw("Failed to renew connection after parameter change", "error", err)
			} else {
				sio.logger.Debug("Renewed connection successfully")
			}
		}
	}()
}

func (sio *SerialIO) maintainConnection(ctx context.Context) {
	backoff := reconnectMinBackoff
	firstAttempt := true

	for {
		if ctx.Err() != nil {
			sio.closeConn()
			return
		}

		opened, err := sio.openAndRead(ctx)
		if ctx.Err() != nil {
			sio.closeConn()
			return
		}

		if opened {
			// Dropped after a live session (sleep/unplug/etc.) — retry quickly
			backoff = reconnectMinBackoff
		}

		if err != nil {
			if firstAttempt && !opened {
				sio.notifyConnectFailure(err)
			}
			sio.logger.Warnw("Serial connection unavailable, will retry",
				"error", err,
				"backoff", backoff.String(),
				"comPort", sio.deej.config.ConnectionInfo.COMPort)
		} else {
			sio.logger.Warn("Serial read loop ended, will reconnect")
		}

		firstAttempt = false
		sio.closeConn()

		forceImmediate := errors.Is(err, errResumeForced)
		wait := backoff
		if forceImmediate {
			wait = 0
			backoff = reconnectMinBackoff
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			sio.closeConn()
			return
		case <-sio.resumeSignal:
			timer.Stop()
			sio.logger.Info("Resume signal received, reconnecting immediately")
			backoff = reconnectMinBackoff
		case <-timer.C:
			if !forceImmediate && !opened {
				backoff *= 2
				if backoff > reconnectMaxBackoff {
					backoff = reconnectMaxBackoff
				}
			}
		}
	}
}

var errResumeForced = errors.New("resume forced reconnect")

func (sio *SerialIO) notifyConnectFailure(err error) {
	port := sio.deej.config.ConnectionInfo.COMPort

	if looksLikeAccessDenied(err) {
		sio.logger.Warnw("Serial port seems busy", "comPort", port, "error", err)
		sio.deej.notifier.Notify(fmt.Sprintf("Can't connect to %s!", port),
			"This serial port is busy, make sure to close any serial monitor or other deej instance. deej will keep retrying.")
		return
	}

	if looksLikeNotExist(err) {
		sio.logger.Warnw("Serial port not found", "comPort", port, "error", err)
		sio.deej.notifier.Notify(fmt.Sprintf("Can't connect to %s!", port),
			"This serial port doesn't exist right now. Check your config — deej will keep retrying (e.g. after wake from sleep).")
		return
	}

	sio.logger.Warnw("Failed to open serial connection", "comPort", port, "error", err)
}

func looksLikeAccessDenied(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access is denied") || strings.Contains(msg, "permission denied") || strings.Contains(msg, "busy")
}

func looksLikeNotExist(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "the system cannot find") ||
		strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "cannot find the file")
}

func (sio *SerialIO) openAndRead(ctx context.Context) (opened bool, err error) {
	// MinimumReadSize 0 on Windows avoids a rare read-congestion lag bug
	minimumReadSize := uint(0)
	if util.Linux() {
		minimumReadSize = 1
	}

	options := serial.OpenOptions{
		PortName:        sio.deej.config.ConnectionInfo.COMPort,
		BaudRate:        uint(sio.deej.config.ConnectionInfo.BaudRate),
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: minimumReadSize,
	}

	sio.logger.Debugw("Attempting serial connection",
		"comPort", options.PortName,
		"baudRate", options.BaudRate,
		"minReadSize", minimumReadSize)

	conn, err := serial.Open(options)
	if err != nil {
		return false, fmt.Errorf("open serial connection: %w", err)
	}

	sio.mu.Lock()
	sio.connOptions = options
	sio.conn = conn
	sio.connected = true
	sio.mu.Unlock()

	namedLogger := sio.logger.Named(strings.ToLower(options.PortName))
	namedLogger.Infow("Connected", "comPort", options.PortName)

	return true, sio.readUntilDisconnect(ctx, namedLogger, conn)
}

func (sio *SerialIO) readUntilDisconnect(ctx context.Context, logger *zap.SugaredLogger, conn io.ReadWriteCloser) error {
	connReader := bufio.NewReader(conn)
	lineChannel := sio.readLine(logger, connReader)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sio.resumeSignal:
			// OS resumed; drop the likely-stale handle and reconnect
			logger.Info("Resume during active connection, forcing reconnect")
			return errResumeForced
		case line, ok := <-lineChannel:
			if !ok {
				return errors.New("serial read loop closed")
			}
			sio.handleLine(logger, line)
		}
	}
}

func (sio *SerialIO) readLine(logger *zap.SugaredLogger, reader *bufio.Reader) chan string {
	ch := make(chan string)

	go func() {
		defer close(ch)

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if sio.deej.Verbose() {
					logger.Warnw("Failed to read line from serial", "error", err, "line", line)
				} else if !errors.Is(err, io.EOF) {
					logger.Warnw("Serial read failed (device likely disconnected)", "error", err)
				}
				return
			}

			if sio.deej.Verbose() {
				logger.Debugw("Read new line", "line", line)
			}

			ch <- line
		}
	}()

	return ch
}

func (sio *SerialIO) closeConn() {
	sio.mu.Lock()
	conn := sio.conn
	sio.conn = nil
	sio.connected = false
	sio.mu.Unlock()

	if conn == nil {
		return
	}

	if err := conn.Close(); err != nil {
		sio.logger.Warnw("Failed to close serial connection", "error", err)
	} else {
		sio.logger.Debug("Serial connection closed")
	}
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {
	if !expectedLinePattern.MatchString(line) {
		return
	}

	line = strings.TrimSuffix(line, "\r\n")
	splitLine := strings.Split(line, "|")
	numSliders := len(splitLine)

	sio.updateSliderCount(logger, numSliders)
	moveEvents := sio.processSliderValues(logger, splitLine)
	sio.deliverMoveEvents(moveEvents)
}

func (sio *SerialIO) updateSliderCount(logger *zap.SugaredLogger, numSliders int) {
	sio.mu.Lock()
	defer sio.mu.Unlock()

	if numSliders != sio.lastKnownNumSliders {
		logger.Infow("Detected sliders", "amount", numSliders)
		sio.lastKnownNumSliders = numSliders
		sio.currentSliderPercentValues = make([]float32, numSliders)

		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
	}
}

func (sio *SerialIO) processSliderValues(logger *zap.SugaredLogger, splitLine []string) []SliderMoveEvent {
	sio.mu.Lock()
	defer sio.mu.Unlock()

	moveEvents := []SliderMoveEvent{}

	if len(sio.currentSliderPercentValues) < len(splitLine) {
		return moveEvents
	}

	for sliderIdx, stringValue := range splitLine {
		number, _ := strconv.Atoi(stringValue)

		if sliderIdx == 0 && number > 1023 {
			logger.Debugw("Got malformed line from serial, ignoring", "line", strings.Join(splitLine, "|"))
			return moveEvents
		}

		normalizedScalar := sio.calculateNormalizedValue(number)

		if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, sio.deej.config.NoiseReductionLevel) {
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar
			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: normalizedScalar,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	return moveEvents
}

func (sio *SerialIO) calculateNormalizedValue(rawValue int) float32 {
	dirtyFloat := float32(rawValue) / 1023.0
	normalizedScalar := util.NormalizeScalar(dirtyFloat)

	if sio.deej.config.InvertSliders {
		normalizedScalar = 1 - normalizedScalar
	}

	return normalizedScalar
}

func (sio *SerialIO) deliverMoveEvents(moveEvents []SliderMoveEvent) {
	if len(moveEvents) == 0 {
		return
	}

	for _, consumer := range sio.sliderMoveConsumers {
		for _, moveEvent := range moveEvents {
			select {
			case consumer <- moveEvent:
			default:
				// drop if consumer is slow; prefer staying responsive on serial
			}
		}
	}
}
