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

package session

import (
	"fmt"

	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/tlsca"

	"github.com/sirupsen/logrus"
)

// Context combines parameters for a database connection session.
type Context struct {
	// ID is the unique session ID.
	ID string
	// Server is the database server handling the connection.
	Server services.DatabaseServer
	// Identity is the identity of the connecting Teleport user.
	Identity tlsca.Identity
	// Checker is the access checker for the identity.
	Checker services.AccessChecker
	// DatabaseUser is the requested database user.
	DatabaseUser string
	// DatabaseName is the requested database name.
	DatabaseName string
	// StartupParameters define initial connection parameters such as date style.
	StartupParameters map[string]string
	// Log is the logger with session specific fields.
	Log logrus.FieldLogger
}

// String returns string representation of the session parameters.
func (c *Context) String() string {
	return fmt.Sprintf("db[%v] identity[%v] dbUser[%v] dbName[%v]",
		c.Server.GetName(), c.Identity.Username, c.DatabaseUser, c.DatabaseName)
}
