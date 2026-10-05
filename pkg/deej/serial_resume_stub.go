//go:build !windows

package deej

func (sio *SerialIO) setupResumeListener() {
	// No OS suspend/resume hook on this platform; reconnect is driven by read failures.
}
