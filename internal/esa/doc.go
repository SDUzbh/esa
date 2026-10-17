// Package esa implements manual intake, shared investigations and reviewed solutions.
// State mutations are transactional; provider calls and runtime work stay outside them.
//
// Store holds the State in memory as persistent Tables with derived indexes,
// commits each Update's changes through PostgresPersister (one writer, fenced
// by a term), and publishes read-only snapshots.
package esa
