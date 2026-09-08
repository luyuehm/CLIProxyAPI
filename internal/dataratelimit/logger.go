package dataratelimit

import "github.com/sirupsen/logrus"

// logger is the package-level logger used by the limiter, blacklist and
// setup. Data-plane rate limiting is runtime infrastructure, so it uses
// logrus like the rest of the gateway.
var logger = logrus.WithField("component", "dataratelimit")
