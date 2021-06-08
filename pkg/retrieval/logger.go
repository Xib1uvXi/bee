package retrieval

import (
	"github.com/ethersphere/bee/pkg/logging"
	"github.com/sirupsen/logrus"
	"os"
)

func (s *Service) initTcLogger() {
	var logger logging.Logger
	logger = logging.New(os.Stdout, logrus.TraceLevel)

	s.tclogger = logger
}
