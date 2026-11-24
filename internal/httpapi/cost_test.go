package httpapi

import (
	"clippet/internal/auth"

	"golang.org/x/crypto/bcrypt"
)

// Every registration and login in this package hashes a password. The hash's
// strength is not under test here, and at the production cost a burst of
// eight registrations under -race on a four-vCPU runner outran the 15 s
// request timeout (TestRegistrationFirstIsAtomicUnderABurst, the nightly
// gate, 2026-11-23). The lowest cost bcrypt accepts keeps the same code paths.
func init() { auth.TestingBcryptCost(bcrypt.MinCost) }
