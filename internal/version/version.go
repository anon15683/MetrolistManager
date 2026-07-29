package version

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type parsed struct {
	components [3]uint64
	alpha      uint64
	stable     bool
}

func Validate(value string) error {
	_, err := parse(value)
	return err
}

func Compare(left, right string) (int, error) {
	l, err := parse(left)
	if err != nil {
		return 0, fmt.Errorf("invalid version %q: %w", left, err)
	}
	r, err := parse(right)
	if err != nil {
		return 0, fmt.Errorf("invalid version %q: %w", right, err)
	}
	for index := range l.components {
		if l.components[index] < r.components[index] {
			return -1, nil
		}
		if l.components[index] > r.components[index] {
			return 1, nil
		}
	}
	if l.stable != r.stable {
		if l.stable {
			return 1, nil
		}
		return -1, nil
	}
	if !l.stable {
		if l.alpha < r.alpha {
			return -1, nil
		}
		if l.alpha > r.alpha {
			return 1, nil
		}
	}
	return 0, nil
}

func parse(value string) (parsed, error) {
	result := parsed{stable: true}
	core, suffix, prerelease := strings.Cut(value, "-")
	if prerelease {
		if len(suffix) < 2 || suffix[0] != 'a' {
			return result, errors.New("prerelease must use -aN")
		}
		alpha := suffix[1:]
		if len(alpha) > 1 && alpha[0] == '0' {
			return result, errors.New("prerelease number is not canonical")
		}
		number, err := strconv.ParseUint(alpha, 10, 64)
		if err != nil {
			return result, errors.New("prerelease number is not a non-negative integer")
		}
		result.alpha = number
		result.stable = false
	}
	parts := strings.Split(core, ".")
	if len(parts) != len(result.components) {
		return result, errors.New("expected MAJOR.MINOR.PATCH")
	}
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return result, errors.New("version component is not canonical")
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return result, errors.New("version component is not a non-negative integer")
		}
		result.components[index] = number
	}
	return result, nil
}
