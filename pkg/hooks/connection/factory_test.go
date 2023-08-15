package connection

import (
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.uber.org/zap"

	"go.keploy.io/server/pkg/hooks/structs"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml"
)

var _ = Describe("Factory", func() {
	var logger *zap.Logger
	BeforeEach(func() {
		logger, _ = zap.NewDevelopmentConfig().Build()
	})

	Describe("When tracker is malformed", func() {
		It("should be able to delete the tracker", func() {
			var inactivityThreshold time.Duration
			factory := NewFactory(inactivityThreshold, logger)
			// TODO: Replace with counterfeiter.
			db := yaml.NewYamlStore(logger)

			dependenciesResetCount := 0

			// Create a new tracker.
			connectionID := structs.ConnID{
				TGID: 1,
				FD:   2,
				TsID: 3,
			}
			tracker := factory.GetOrCreate(connectionID)
			Expect(tracker).To(Not(BeNil()))
			Expect(tracker).To(Equal(factory.connections[connectionID]))

			// Intentionally corrupt the recvBuf of the tracker.
			tracker.recvBuf = []byte("Intentionally corrupted recvBuf")
			models.SetMode(models.MODE_RECORD)
			MarkTrackerAsMalformed(tracker)
			factory.HandleReadyConnections("fake-path", db)
			Expect(dependenciesResetCount).To(Equal(0))
		})
	})
})

func MarkTrackerAsComplete(tracker *Tracker) {
	// Mark the tracker as complete.
	tracker.totalReadBytes = 1
	tracker.recvBytes = tracker.totalReadBytes

	tracker.totalWrittenBytes = 2
	tracker.sentBytes = tracker.totalWrittenBytes

	tracker.closeTimestamp = uint64(time.Now().UnixNano())
}

func MarkTrackerAsMalformed(tracker *Tracker) {
	corruptionType := rand.Intn(3)
	switch corruptionType {
	case 0:
		tracker.closeTimestamp = 0
	case 1:
		tracker.totalReadBytes = 1
		tracker.recvBytes = 2
	case 2:
		tracker.totalWrittenBytes = 3
		tracker.sentBytes = 4
	}
}
