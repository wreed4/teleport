/*
Copyright 2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package types

import (
	"fmt"
	"testing"
	"time"

	"github.com/gravitational/teleport/lib/fixtures"
	"github.com/gravitational/teleport/lib/utils"

	"gopkg.in/check.v1"
)

type ResetPasswordTokenSuite struct{}

var _ = check.Suite(&ResetPasswordTokenSuite{})
var _ = fmt.Printf

func (s *ResetPasswordTokenSuite) SetUpSuite(c *check.C) {
	utils.InitLoggerForTests(testing.Verbose())
}

func (s *ResetPasswordTokenSuite) TestUnmarshal(c *check.C) {
	created, err := time.Parse(time.RFC3339, "2020-01-14T18:52:39.523076855Z")
	c.Assert(err, check.IsNil)

	type testCase struct {
		description string
		input       string
		expected    ResetPasswordToken
	}

	testCases := []testCase{
		{
			description: "simple case",
			input: `
        {
          "kind": "user_token",
          "version": "v3",
          "metadata": {
            "name": "tokenId"
          },
          "spec": {
            "user": "example@example.com",
            "created": "2020-01-14T18:52:39.523076855Z",
            "url": "https://localhost"
          }
        }
      `,
			expected: &ResetPasswordTokenV3{
				Kind:    KindResetPasswordToken,
				Version: V3,
				Metadata: Metadata{
					Name: "tokenId",
				},
				Spec: ResetPasswordTokenSpecV3{
					Created: created,
					User:    "example@example.com",
					URL:     "https://localhost",
				},
			},
		},
	}

	marshaler := GetResetPasswordTokenMarshaler()
	for _, tc := range testCases {
		comment := check.Commentf("test case %q", tc.description)
		out, err := marshaler.Unmarshal([]byte(tc.input))
		c.Assert(err, check.IsNil, comment)
		fixtures.DeepCompare(c, tc.expected, out)
		data, err := marshaler.Marshal(out)
		c.Assert(err, check.IsNil, comment)
		out2, err := marshaler.Unmarshal(data)
		c.Assert(err, check.IsNil, comment)
		fixtures.DeepCompare(c, tc.expected, out2)
	}
}
