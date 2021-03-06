/*
 * Copyright (C) 2022 Nuts community
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 *
 */

package util

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWrapError(t *testing.T) {
	var original = errors.New("original")
	var cause = errors.New("cause")
	wrapped := WrapError(original, cause)
	assert.ErrorIs(t, wrapped, original)
	assert.ErrorIs(t, wrapped, cause)
}

func Test_wrappedError_Error(t *testing.T) {
	wrapped := WrapError(errors.New("original"), errors.New("cause"))
	assert.EqualError(t, wrapped, "original: cause")
}
