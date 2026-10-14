package tls

// testCA is a single generated MITM CA shared across the unit tests. It replaces
// the retired, committed caCrt/caPKey globals: tests that used to byte-compare
// against the embedded static cert now compare against this generated one's
// certPEM/der. Generated once per test binary so comparisons within a test are
// self-consistent.
var testCA = mustGenerateTestCA()

func mustGenerateTestCA() *caState {
	ca, err := generateRunCA()
	if err != nil {
		panic("generateRunCA failed in test setup: " + err.Error())
	}
	return ca
}

// testCACertPEM / testCADER stand in for the old caCrt bytes and its DER.
func testCACertPEM() []byte { return testCA.certPEM }
func testCADER() []byte     { return testCA.der }
