package daemon

import (
	"os"
	"os/signal"
	"syscall"
)

func watchSignals(on func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	signal.Stop(ch)
	on()
}
