package permify

import (
	"errors"
	"testing"
)

func TestAlreadyExistsCoversBothPermifyShapes(t *testing.T) {
	if !alreadyExists(errors.New(`permify /v1/tenants/create: status 400: {"code":3, "message":"ERROR_CODE_UNIQUE_CONSTRAINT"}`)) {
		t.Fatal("400 unique-constraint must count as already exists")
	}
	if !alreadyExists(errors.New("permify /v1/tenants/create: status 409: conflict")) {
		t.Fatal("409 must count as already exists")
	}
	if alreadyExists(errors.New("permify /v1/tenants/create: status 500: boom")) {
		t.Fatal("a real failure must not be swallowed")
	}
}
