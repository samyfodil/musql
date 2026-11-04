package cli

import "golang.org/x/sys/windows"

// consoleVT has the console behind fd interpret the escape sequences the line
// editor writes to move the cursor and erase: a Windows console prints them
// as text unless told to, which put "^[[D" on the screen for every backspace
// and arrow key (#93). It answers the undo, and does nothing for a descriptor
// that is not a console or a console that cannot.
func consoleVT(fd int) (restore func()) {
	h := windows.Handle(fd)
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return func() {}
	}
	if windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
		return func() {}
	}
	return func() { windows.SetConsoleMode(h, mode) }
}
