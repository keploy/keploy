package proxy

import (
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.uber.org/zap"
)

var _ = Describe("Proxy", func() {
	var logger *zap.Logger
	BeforeEach(func() {
		logger, _ = zap.NewDevelopmentConfig().Build()
	})

	Describe("When the exact port is available", func() {
		It("should boot up the proxy on that port", func() {
			// Select a random port which is free.
			listener, err := net.Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}

			port := uint32(listener.Addr().(*net.TCPAddr).Port)
			listener.Close()

			proxySet := BootProxies(logger, Option{StartingPort: port, Count: 1})
			Expect(proxySet).To(Not(BeNil()))
			Expect(len(proxySet.PortList)).To(Equal(1))
			Expect(proxySet.PortList[0]).To(Equal(port))
		})
	})

	Describe("When the port is busy", func() {
		It("should boot up the proxy on the next port instead of terminating", func() {
			// Select a random port which is free. Block the connection on it by not closing the port.
			listener, err := net.Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}

			port := uint32(listener.Addr().(*net.TCPAddr).Port)
			proxySet := BootProxies(logger, Option{StartingPort: port, Count: 1})
			Expect(proxySet).To(Not(BeNil()))
			Expect(len(proxySet.PortList)).To(Equal(1))
			Expect(proxySet.PortList[0]).To(Not(Equal(port)))

			// Once the test case completes, free up the port.
			listener.Close()
		})
	})
})
