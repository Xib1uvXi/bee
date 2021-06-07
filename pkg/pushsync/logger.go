package pushsync

import (
	"github.com/ethersphere/bee/pkg/logging"
	"github.com/sirupsen/logrus"
	"os"
)

func (ps *PushSync) initTcLogger() {
	var logger logging.Logger
	logger = logging.New(os.Stdout, logrus.TraceLevel)

	ps.tclogger = logger
}
