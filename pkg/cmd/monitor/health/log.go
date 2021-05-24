package health

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

func (c *Check) logHealthFail(err error) {
	if !c.isDisruption() {
		c.status.disruption = true
		c.lg.Info(
			"service disruption detected",
			zap.Strings("addresses", c.targets),
			zap.Any("check", c.name),
			zap.Bool("health", false),
			zap.Time("start", c.status.logDisruptionStart),
			zap.Error(err),
		)
		return
	}
	//sanitize context error
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("client timeout exceeded: %s", DefaultContextDeadline)
	}
	c.lg.Info("health check",
		zap.Strings("addresses", c.targets),
		zap.Any("check", c.name),
		zap.Bool("health", false),
		zap.Time("start", c.status.logDisruptionStart),
		zap.Error(err),
	)
	return
}

func (c *Check) logSlowRequest(err error) {
	if !c.isDisruption() {
		c.lg.Info(
			"possible service disruption detected",
			zap.Strings("addresses", c.targets),
			zap.Any("check", c.name),
			zap.Bool("health", false),
			zap.Time("start", c.status.logDisruptionStart),
			zap.Error(err),
		)
	}
	return
}

func (c *Check) logHealthSuccess() {
	if c.isDisruption() {
		c.status.logDisruptionEnd = time.Now()
		duration := time.Since(c.status.logDisruptionStart)
		c.lg.Info(
			"service restored",
			zap.Strings("addresses", c.targets),
			zap.Any("check", c.name),
			zap.Bool("health", true),
			zap.Time("start", c.status.logDisruptionStart),
			zap.Time("end", c.status.logDisruptionEnd),
			zap.Duration("duration", duration),
		)
		// reset status
		c.status.disruption = false
		c.status.slowRequest = false
		c.status.logDisruptionStart = time.Time{}
		c.status.logDisruptionEnd = time.Time{}
		return
	}
	c.status.slowRequest = false
	c.lg.Debug("health check",
		zap.Strings("addresses", c.targets),
		zap.Any("check", c.name),
		zap.Bool("health", true),
	)
	return
}

func (c *Check) LogTermination() {
	// close etcd client
	c.client.Close()
	if c.isDisruption() || c.isSlowRequest() {
		c.status.logDisruptionEnd = time.Now()
		duration := time.Since(c.status.logDisruptionStart)
		c.lg.Warn("health probe terminated",
			zap.Strings("addresses", c.targets),
			zap.Any("check", c.name),
			zap.Bool("health", false),
			zap.Time("start", c.status.logDisruptionStart),
			zap.Time("end", c.status.logDisruptionEnd),
			zap.Duration("duration", duration),
		)
		return
	}
	c.lg.Info("health probe terminated",
		zap.Strings("addresses", c.targets),
		zap.Any("check", c.name),
		zap.Bool("health", true),
	)
	return
}

func (c *Check) isDisruption() bool {
	return c.status.disruption
}

func (c *Check) isSlowRequest() bool {
	return c.status.slowRequest
}
