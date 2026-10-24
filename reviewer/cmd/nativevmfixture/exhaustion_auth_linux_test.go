//go:build linux

package main

// returnedAuth sets whether a case's synthetic auth came back returned and
// usable, as the runner derives it from the store, the audit and the error.
func returnedAuth(in exhaustionInput, returned bool) exhaustionInput {
	in.AuthReturned = returned
	return in
}
