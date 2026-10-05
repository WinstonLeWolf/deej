package deej

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"syscall"

	ole "github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"go.uber.org/zap"
)

type NotificationClient struct {
	vtbl *NotificationClientVtbl
}

type NotificationClientVtbl struct {
	QueryInterface         uintptr
	AddRef                 uintptr
	Release                uintptr
	OnDeviceStateChanged   uintptr
	OnDeviceAdded          uintptr
	OnDeviceRemoved        uintptr
	OnDefaultDeviceChanged uintptr
	OnPropertyValueChanged uintptr
}

func NewNotificationClient(sf *wcaSessionFinder) *NotificationClient {
	client := &NotificationClient{
		vtbl: &NotificationClientVtbl{
			QueryInterface:         syscall.NewCallback(sf.noopCallback),
			AddRef:                 syscall.NewCallback(sf.noopCallback),
			Release:                syscall.NewCallback(sf.noopCallback),
			OnDeviceStateChanged:   syscall.NewCallback(sf.noopCallback),
			OnDeviceAdded:          syscall.NewCallback(sf.noopCallback),
			OnDeviceRemoved:        syscall.NewCallback(sf.noopCallback),
			OnPropertyValueChanged: syscall.NewCallback(sf.noopCallback),
			OnDefaultDeviceChanged: syscall.NewCallback(sf.defaultDeviceChangedCallback),
		},
	}
	return client
}

type wcaSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	eventCtx *ole.GUID // needed for some session actions to successfully notify other audio consumers

	// needed for device change notifications
	mmDeviceEnumerator      *wca.IMMDeviceEnumerator
	mmNotificationClient    *wca.IMMNotificationClient
	lastDefaultDeviceChange time.Time

	// our master input and output sessions
	masterOut *masterSession
	masterIn  *masterSession

	ctx    context.Context
	cancel context.CancelFunc
}

type audioDevice struct {
	endpoint     *wca.IMMDevice
	description  string
	friendlyName string
	dataFlow     uint32
}

const (

	// there's no real mystery here, it's just a random GUID
	myteriousGUID = "{1ec920a1-7db8-44ba-9779-e5d28ed9f330}"

	// the notification client will call this multiple times in quick succession based on the
	// default device's assigned media roles, so we need to filter out the extraneous calls
	minDefaultDeviceChangeThreshold = 100 * time.Millisecond

	// prefix for device sessions in logger
	deviceSessionFormat = "device.%s"
)

const (
	comSFalse             = 0x00000001
	systemSoundsErrorCode = 143196173
	maxRetries            = 3
	retryDelay            = 100 * time.Millisecond
)

func withRetry(operation func() error) error {
	var lastErr error
	for range maxRetries {
		if err := operation(); err != nil {
			lastErr = err
			time.Sleep(retryDelay)
			continue
		}
		return nil
	}
	return fmt.Errorf("operation failed after %d retries: %w", maxRetries, lastErr)
}

func newSessionFinder(logger *zap.SugaredLogger) (SessionFinder, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Always clean up the context if we return an error
	var cleanup bool
	defer func() {
		if cleanup {
			cancel()
		}
	}()

	eventCtx := ole.NewGUID(myteriousGUID)
	if eventCtx == nil {
		cleanup = true
		return nil, fmt.Errorf("failed to create event context GUID")
	}

	sf := &wcaSessionFinder{
		logger:        logger.Named("session_finder"),
		sessionLogger: logger.Named("sessions"),
		eventCtx:      eventCtx,
		ctx:           ctx,
		cancel:        cancel,
	}

	// Start connection monitoring
	sf.monitorConnection()

	sf.logger.Debug("Created WCA session finder instance")
	return sf, nil
}

func (sf *wcaSessionFinder) initializeCOM() error {
	err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)
	if err == nil {
		return nil
	}

	if oleErr, ok := err.(*ole.OleError); ok {
		switch oleErr.Code() {
		case comSFalse: // S_FALSE
			sf.logger.Debug("COM already initialized")
			return nil
		case ole.E_INVALIDARG:
			sf.logger.Debug("Retrying COM initialization with single-threaded model")
			if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
				return fmt.Errorf("failed to initialize COM in single-threaded mode: %w", err)
			}
			return nil
		}
	}

	return fmt.Errorf("failed to initialize COM: %w", err)
}

func (sf *wcaSessionFinder) setupMasterSessions(defaultOutputEndpoint, defaultInputEndpoint *wca.IMMDevice) ([]Session, error) {
	var sessions []Session
	var err error

	sf.masterOut, err = sf.getMasterSession(defaultOutputEndpoint, masterSessionName, masterSessionName)
	if err != nil {
		sf.logger.Warnw("Failed to get master audio output session", "error", err)
		return nil, fmt.Errorf("get master audio output session: %w", err)
	}
	sessions = append(sessions, sf.masterOut)

	if defaultInputEndpoint != nil {
		sf.masterIn, err = sf.getMasterSession(defaultInputEndpoint, inputSessionName, inputSessionName)
		if err != nil {
			sf.logger.Warnw("Failed to get master audio input session", "error", err)
			return nil, fmt.Errorf("get master audio input session: %w", err)
		}
		sessions = append(sessions, sf.masterIn)
	}

	return sessions, nil
}

func (sf *wcaSessionFinder) GetAllSessions() ([]Session, error) {
	cleanup := func(sessions []Session) {
		for _, session := range sessions {
			session.Release()
		}
	}

	sessions, err := sf.getAllSessionsInternal()
	if err != nil {
		cleanup(sessions)
		return nil, err
	}

	return sessions, nil
}

func (sf *wcaSessionFinder) getAllSessionsInternal() ([]Session, error) {
	if err := sf.initializeCOM(); err != nil {
		return nil, fmt.Errorf("initialize COM: %w", err)
	}

	// Check connection state and attempt to connect if needed
	if !sf.isConnected() {
		if err := sf.getDeviceEnumerator(); err != nil {
			return nil, fmt.Errorf("get device enumerator: %w", err)
		}

		if err := sf.registerDefaultDeviceChangeCallback(); err != nil {
			return nil, fmt.Errorf("register device notifications: %w", err)
		}
	}

	defaultOutputEndpoint, defaultInputEndpoint, err := sf.getDefaultAudioEndpoints()
	if err != nil {
		sf.logger.Warnw("Failed to get default audio endpoints", "error", err)
		return nil, fmt.Errorf("get default audio endpoints: %w", err)
	}
	defer func() {
		if defaultOutputEndpoint != nil {
			defaultOutputEndpoint.Release()
		}
		if defaultInputEndpoint != nil {
			defaultInputEndpoint.Release()
		}
	}()

	sessions, err := sf.setupMasterSessions(defaultOutputEndpoint, defaultInputEndpoint)
	if err != nil {
		return nil, err
	}

	if err := sf.enumerateAndAddSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate device sessions", "error", err)
		return nil, fmt.Errorf("enumerate device sessions: %w", err)
	}

	return sessions, nil
}

func (sf *wcaSessionFinder) Release() error {
	// Cancel context first to stop background tasks
	sf.cancel()

	if sf.isConnected() {
		if sf.mmNotificationClient != nil {
			_ = sf.mmDeviceEnumerator.UnregisterEndpointNotificationCallback(sf.mmNotificationClient)
			sf.mmNotificationClient = nil
		}

		sf.mmDeviceEnumerator.Release()
		sf.mmDeviceEnumerator = nil
	}

	// Release master sessions if they exist
	if sf.masterOut != nil {
		sf.masterOut.Release()
	}
	if sf.masterIn != nil {
		sf.masterIn.Release()
	}

	ole.CoUninitialize()
	sf.logger.Debug("Released WCA session finder instance")
	return nil
}

func (sf *wcaSessionFinder) getDeviceEnumerator() error {

	// get the IMMDeviceEnumerator (only once)
	if sf.mmDeviceEnumerator == nil {
		if err := wca.CoCreateInstance(
			wca.CLSID_MMDeviceEnumerator,
			0,
			wca.CLSCTX_ALL,
			wca.IID_IMMDeviceEnumerator,
			&sf.mmDeviceEnumerator,
		); err != nil {
			sf.logger.Warnw("Failed to call CoCreateInstance", "error", err)
			return fmt.Errorf("call CoCreateInstance: %w", err)
		}
	}

	return nil
}

func (sf *wcaSessionFinder) getDefaultAudioEndpoints() (*wca.IMMDevice, *wca.IMMDevice, error) {

	// get the default audio endpoints as IMMDevice instances
	var mmOutDevice *wca.IMMDevice
	var mmInDevice *wca.IMMDevice

	if err := sf.mmDeviceEnumerator.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &mmOutDevice); err != nil {
		sf.logger.Warnw("Failed to call GetDefaultAudioEndpoint (out)", "error", err)
		return nil, nil, fmt.Errorf("call GetDefaultAudioEndpoint (out): %w", err)
	}

	// allow this call to fail (not all users have a microphone connected)
	if err := sf.mmDeviceEnumerator.GetDefaultAudioEndpoint(wca.ECapture, wca.EConsole, &mmInDevice); err != nil {
		sf.logger.Warn("No default input device detected, proceeding without it (\"mic\" will not work)")
		mmInDevice = nil
	}

	return mmOutDevice, mmInDevice, nil
}

// NOTE: Updated to work with the new go-wca package v0.3.0
// registerDefaultDeviceChangeCallback registers the default device change callback
// with the MMDeviceEnumerator. This is called when the default audio device
// changes, and it will mark the master sessions as stale.
// This is a workaround for the fact that the default device change callback
// is not implemented in go-wca. The callback is called from the
// IMMNotificationClient interface, which is implemented in the wca package.
func (sf *wcaSessionFinder) registerDefaultDeviceChangeCallback() error {
	notificationClient := NewNotificationClient(sf)
	sf.mmNotificationClient = (*wca.IMMNotificationClient)(unsafe.Pointer(notificationClient))

	if err := sf.mmDeviceEnumerator.RegisterEndpointNotificationCallback(sf.mmNotificationClient); err != nil {
		sf.logger.Warnw("Failed to call RegisterEndpointNotificationCallback", "error", err)
		return fmt.Errorf("call RegisterEndpointNotificationCallback: %w", err)
	}

	return nil
}

func (sf *wcaSessionFinder) getMasterSession(mmDevice *wca.IMMDevice, key string, loggerKey string) (*masterSession, error) {

	var audioEndpointVolume *wca.IAudioEndpointVolume

	if err := mmDevice.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &audioEndpointVolume); err != nil {
		sf.logger.Warnw("Failed to activate AudioEndpointVolume for master session", "error", err)
		return nil, fmt.Errorf("activate master session: %w", err)
	}

	// create the master session
	master, err := newMasterSession(sf.sessionLogger, audioEndpointVolume, sf.eventCtx, key, loggerKey)
	if err != nil {
		sf.logger.Warnw("Failed to create master session instance", "error", err)
		return nil, fmt.Errorf("create master session: %w", err)
	}

	return master, nil
}

func (sf *wcaSessionFinder) processDevice(deviceIdx uint32, deviceCollection *wca.IMMDeviceCollection, sessions *[]Session) error {
	if deviceCollection == nil || sessions == nil {
		return fmt.Errorf("nil device collection or sessions slice")
	}

	var endpoint *wca.IMMDevice
	if err := deviceCollection.Item(deviceIdx, &endpoint); err != nil {
		return fmt.Errorf("get device %d from collection: %w", deviceIdx, err)
	}
	if endpoint == nil {
		return fmt.Errorf("got nil endpoint for device %d", deviceIdx)
	}
	defer endpoint.Release()

	deviceInfo, err := sf.getDeviceInfo(deviceIdx, endpoint)
	if err != nil {
		// Broken/ghost endpoints (common with HDMI/NVIDIA devices after sleep) used to
		// fail the entire init and prevent the tray from ever appearing. Skip them.
		sf.logger.Warnw("Skipping audio device that failed info lookup",
			"deviceIdx", deviceIdx,
			"error", err)
		return nil
	}

	return sf.handleDevice(deviceIdx, deviceInfo, sessions)
}

func (sf *wcaSessionFinder) handleDevice(deviceIdx uint32, deviceInfo *audioDevice, sessions *[]Session) error {
	sf.logger.Debugw("Enumerated device info",
		"deviceIdx", deviceIdx,
		"deviceDescription", deviceInfo.description,
		"deviceFriendlyName", deviceInfo.friendlyName,
		"dataFlow", deviceInfo.dataFlow)

	if deviceInfo.dataFlow == wca.ERender {
		if err := sf.enumerateAndAddProcessSessions(deviceInfo.endpoint, deviceInfo.friendlyName, sessions); err != nil {
			sf.logger.Warnw("Failed to enumerate and add process sessions for device", "deviceIdx", deviceIdx, "error", err)
			return fmt.Errorf("enumerate and add device %d process sessions: %w", deviceIdx, err)
		}
	}

	newSession, err := sf.getMasterSession(deviceInfo.endpoint,
		deviceInfo.friendlyName,
		fmt.Sprintf(deviceSessionFormat, deviceInfo.description))
	if err != nil {
		sf.logger.Warnw("Failed to get master session for device", "deviceIdx", deviceIdx, "error", err)
		return fmt.Errorf("get device %d master session: %w", deviceIdx, err)
	}

	*sessions = append(*sessions, newSession)
	return nil
}

func (sf *wcaSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {
	return withRetry(func() error {
		var deviceCollection *wca.IMMDeviceCollection
		if err := sf.mmDeviceEnumerator.EnumAudioEndpoints(wca.EAll, wca.DEVICE_STATE_ACTIVE, &deviceCollection); err != nil {
			return fmt.Errorf("enumerate active audio endpoints: %w", err)
		}
		defer deviceCollection.Release()

		var deviceCount uint32
		if err := deviceCollection.GetCount(&deviceCount); err != nil {
			sf.logger.Warnw("Failed to get device count from device collection", "error", err)
			return fmt.Errorf("get device count from device collection: %w", err)
		}

		for deviceIdx := uint32(0); deviceIdx < deviceCount; deviceIdx++ {
			if err := sf.processDevice(deviceIdx, deviceCollection, sessions); err != nil {
				return err
			}
		}

		return nil
	})
}

func (sf *wcaSessionFinder) enumerateAndAddProcessSessions(
	endpoint *wca.IMMDevice,
	endpointFriendlyName string,
	sessions *[]Session,
) error {
	sf.logger.Debugw("Enumerating and adding process sessions for audio output device",
		"deviceFriendlyName", endpointFriendlyName)

	sessionEnumerator, err := sf.getSessionEnumerator(endpoint)
	if err != nil {
		return err
	}
	defer sessionEnumerator.Release()

	return sf.processAudioSessions(sessionEnumerator, sessions)
}

func (sf *wcaSessionFinder) getSessionEnumerator(endpoint *wca.IMMDevice) (*wca.IAudioSessionEnumerator, error) {
	var audioSessionManager2 *wca.IAudioSessionManager2
	if err := endpoint.Activate(
		wca.IID_IAudioSessionManager2,
		wca.CLSCTX_ALL,
		nil,
		&audioSessionManager2,
	); err != nil {
		sf.logger.Warnw("Failed to activate endpoint as IAudioSessionManager2", "error", err)
		return nil, fmt.Errorf("activate endpoint: %w", err)
	}
	defer audioSessionManager2.Release()

	var sessionEnumerator *wca.IAudioSessionEnumerator
	if err := audioSessionManager2.GetSessionEnumerator(&sessionEnumerator); err != nil {
		return nil, err
	}

	return sessionEnumerator, nil
}

func (sf *wcaSessionFinder) processAudioSessions(sessionEnumerator *wca.IAudioSessionEnumerator, sessions *[]Session) error {
	var sessionCount int
	if err := sessionEnumerator.GetCount(&sessionCount); err != nil {
		sf.logger.Warnw("Failed to get session count from session enumerator", "error", err)
		return fmt.Errorf("get session count: %w", err)
	}

	sf.logger.Debugw("Got session count from session enumerator", "count", sessionCount)

	for sessionIdx := range sessionCount {
		if err := sf.processSession(sessionIdx, sessionEnumerator, sessions); err != nil {
			return err
		}
	}

	return nil
}

func (sf *wcaSessionFinder) processSession(sessionIdx int, sessionEnumerator *wca.IAudioSessionEnumerator, sessions *[]Session) error {
	audioSessionControl2, err := sf.getAudioSessionControl2(sessionIdx, sessionEnumerator)
	if err != nil {
		return err
	}

	simpleAudioVolume, err := sf.getSimpleAudioVolume(sessionIdx, audioSessionControl2)
	if err != nil {
		audioSessionControl2.Release()
		return err
	}

	pid, err := sf.getProcessId(sessionIdx, audioSessionControl2)
	if err != nil {
		audioSessionControl2.Release()
		simpleAudioVolume.Release()
		return err
	}

	newSession, err := newWCASession(sf.sessionLogger, audioSessionControl2, simpleAudioVolume, pid, sf.eventCtx)
	if err != nil {
		if !errors.Is(err, errNoSuchProcess) {
			sf.logger.Warnw("Failed to create new WCA session instance",
				"error", err,
				"sessionIdx", sessionIdx)
			return fmt.Errorf("create wca session for session %d: %w", sessionIdx, err)
		}

		sf.logger.Debugw("Process already exited, skipping session and releasing handles", "pid", pid)
		audioSessionControl2.Release()
		simpleAudioVolume.Release()
		return nil
	}

	*sessions = append(*sessions, newSession)
	return nil
}

func (sf *wcaSessionFinder) getAudioSessionControl2(sessionIdx int, sessionEnumerator *wca.IAudioSessionEnumerator) (*wca.IAudioSessionControl2, error) {
	var audioSessionControl *wca.IAudioSessionControl
	if err := sessionEnumerator.GetSession(sessionIdx, &audioSessionControl); err != nil {
		sf.logger.Warnw("Failed to get session from session enumerator",
			"error", err,
			"sessionIdx", sessionIdx)
		return nil, fmt.Errorf("get session %d from enumerator: %w", sessionIdx, err)
	}
	defer audioSessionControl.Release()

	dispatch, err := audioSessionControl.QueryInterface(wca.IID_IAudioSessionControl2)
	if err != nil {
		sf.logger.Warnw("Failed to query session's IAudioSessionControl2",
			"error", err,
			"sessionIdx", sessionIdx)
		return nil, fmt.Errorf("query session %d IAudioSessionControl2: %w", sessionIdx, err)
	}

	return (*wca.IAudioSessionControl2)(unsafe.Pointer(dispatch)), nil
}

func (sf *wcaSessionFinder) getSimpleAudioVolume(sessionIdx int, audioSessionControl2 *wca.IAudioSessionControl2) (*wca.ISimpleAudioVolume, error) {
	dispatch, err := audioSessionControl2.QueryInterface(wca.IID_ISimpleAudioVolume)
	if err != nil {
		sf.logger.Warnw("Failed to query session's ISimpleAudioVolume",
			"error", err,
			"sessionIdx", sessionIdx)
		return nil, fmt.Errorf("query session %d ISimpleAudioVolume: %w", sessionIdx, err)
	}

	return (*wca.ISimpleAudioVolume)(unsafe.Pointer(dispatch)), nil
}

func (sf *wcaSessionFinder) getProcessId(sessionIdx int, audioSessionControl2 *wca.IAudioSessionControl2) (uint32, error) {
	var pid uint32
	if err := audioSessionControl2.GetProcessId(&pid); err != nil {
		isSystemSoundsErr := audioSessionControl2.IsSystemSoundsSession()
		if isSystemSoundsErr != nil && !strings.Contains(err.Error(), fmt.Sprintf("%d", systemSoundsErrorCode)) {
			sf.logger.Warnw("Failed to query session's pid",
				"error", err,
				"isSystemSoundsError", isSystemSoundsErr,
				"sessionIdx", sessionIdx)
			return 0, fmt.Errorf("query session %d pid: %w", sessionIdx, err)
		}
	}
	return pid, nil
}

func (sf *wcaSessionFinder) defaultDeviceChangedCallback(
	this *wca.IMMNotificationClient,
	EDataFlow, eRole uint32,
	lpcwstr uintptr,
) (hResult uintptr) {

	now := time.Now()

	if sf.lastDefaultDeviceChange.Add(minDefaultDeviceChangeThreshold).After(now) {
		return
	}

	sf.lastDefaultDeviceChange = now

	sf.logger.Debug("Default audio device changed, marking master sessions as stale")
	if sf.masterOut != nil {
		sf.masterOut.markAsStale()
	}

	if sf.masterIn != nil {
		sf.masterIn.markAsStale()
	}

	return
}
func (sf *wcaSessionFinder) noopCallback() (hResult uintptr) {
	return
}

func (sf *wcaSessionFinder) isConnected() bool {
	return sf.mmDeviceEnumerator != nil && sf.mmNotificationClient != nil
}

func (sf *wcaSessionFinder) getDeviceInfo(_ uint32, endpoint *wca.IMMDevice) (*audioDevice, error) {
	var propertyStore *wca.IPropertyStore
	if err := endpoint.OpenPropertyStore(wca.STGM_READ, &propertyStore); err != nil {
		return nil, fmt.Errorf("open endpoint property store: %w", err)
	}
	defer propertyStore.Release()

	value := &wca.PROPVARIANT{}
	if err := propertyStore.GetValue(&wca.PKEY_Device_DeviceDesc, value); err != nil {
		return nil, fmt.Errorf("get device description: %w", err)
	}
	description := strings.ToLower(value.String())

	if err := propertyStore.GetValue(&wca.PKEY_Device_FriendlyName, value); err != nil {
		return nil, fmt.Errorf("get device friendly name: %w", err)
	}
	friendlyName := value.String()

	dispatch, err := endpoint.QueryInterface(wca.IID_IMMEndpoint)
	if err != nil {
		return nil, fmt.Errorf("query IMMEndpoint: %w", err)
	}

	endpointType := (*wca.IMMEndpoint)(unsafe.Pointer(dispatch))
	defer endpointType.Release()

	var dataFlow uint32
	if err := endpointType.GetDataFlow(&dataFlow); err != nil {
		return nil, fmt.Errorf("get data flow: %w", err)
	}

	return &audioDevice{
		endpoint:     endpoint,
		description:  description,
		friendlyName: friendlyName,
		dataFlow:     dataFlow,
	}, nil
}

func (sf *wcaSessionFinder) monitorConnection() {
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-sf.ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				if !sf.isConnected() {
					sf.logger.Warn("Connection lost, attempting to reconnect...")
					if _, err := sf.getAllSessionsInternal(); err != nil {
						sf.logger.Errorw("Failed to reconnect", "error", err)
					}
				}
			}
		}
	}()
}
