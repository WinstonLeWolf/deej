//go:build windows

package deej

// setupResumeListener intentionally does not register PowerRegisterSuspendResumeNotification.
// A previous DEVICE_NOTIFY_CALLBACK registration caused heap corruption on some systems
// (callback/params lifetime issues with the Windows power API).
// Hibernate/sleep recovery is handled by the serial maintain loop: read failures close the
// port and reconnect with backoff.
func (sio *SerialIO) setupResumeListener() {
	sio.logger.Debug("Using serial read-failure reconnect for sleep/hibernate recovery")
}
