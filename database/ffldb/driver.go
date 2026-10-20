// Copyright (c) 2015-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package ffldb

import (
	"fmt"

	"github.com/btcsuite/btclog"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/wire"
)

var log = btclog.Disabled

const (
	dbType = "ffldb"
)

// parseArgs parses the arguments from the database Open/Create methods.
func parseArgs(funcName string, args ...interface{}) (string, wire.BitcoinNet, Options, error) {
	if len(args) != 2 && len(args) != 3 {
		return "", 0, Options{}, fmt.Errorf("invalid arguments to %s.%s -- "+
			"expected database path, block network, and optional ffldb.Options", dbType,
			funcName)
	}

	dbPath, ok := args[0].(string)
	if !ok {
		return "", 0, Options{}, fmt.Errorf("first argument to %s.%s is invalid -- "+
			"expected database path string", dbType, funcName)
	}

	network, ok := args[1].(wire.BitcoinNet)
	if !ok {
		return "", 0, Options{}, fmt.Errorf("second argument to %s.%s is invalid -- "+
			"expected block network", dbType, funcName)
	}

	var options Options
	if len(args) == 3 {
		options, ok = args[2].(Options)
		if !ok {
			return "", 0, Options{}, fmt.Errorf("third argument to %s.%s is invalid -- "+
				"expected ffldb.Options", dbType, funcName)
		}
		if err := options.validate(); err != nil {
			return "", 0, Options{}, err
		}
	}
	return dbPath, network, options, nil
}

// openDBDriver is the callback provided during driver registration that opens
// an existing database for use.
func openDBDriver(args ...interface{}) (database.DB, error) {
	dbPath, network, options, err := parseArgs("Open", args...)
	if err != nil {
		return nil, err
	}

	return openDB(dbPath, network, false, options)
}

// createDBDriver is the callback provided during driver registration that
// creates, initializes, and opens a database for use.
func createDBDriver(args ...interface{}) (database.DB, error) {
	dbPath, network, options, err := parseArgs("Create", args...)
	if err != nil {
		return nil, err
	}

	return openDB(dbPath, network, true, options)
}

// useLogger is the callback provided during driver registration that sets the
// current logger to the provided one.
func useLogger(logger btclog.Logger) {
	log = logger
}

func init() {
	// Register the driver.
	driver := database.Driver{
		DbType:    dbType,
		Create:    createDBDriver,
		Open:      openDBDriver,
		UseLogger: useLogger,
	}
	if err := database.RegisterDriver(driver); err != nil {
		panic(fmt.Sprintf("Failed to regiser database driver '%s': %v",
			dbType, err))
	}
}
