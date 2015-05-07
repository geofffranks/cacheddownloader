package cacheddownloader_test

import (
	"time"

	. "github.com/pivotal-golang/cacheddownloader"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("DownloadCancelledError", func() {
	It("reports an error with source, duration and bytes", func() {
		e := NewDownloadCancelledError("here", 30*time.Second, 1)
		Expect(e.Error()).To(Equal("Download cancelled: source 'here', duration '30s', bytes '1'"))
	})

	Context("when no bytes have been read", func() {
		It("only reports source and duration", func() {
			e := NewDownloadCancelledError("here", 30*time.Second, NoBytesReceived)
			Expect(e.Error()).To(Equal("Download cancelled: source 'here', duration '30s'"))
		})
	})
})