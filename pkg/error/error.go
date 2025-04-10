//  Copyright (c) 2025 dingodb.com, Inc. All Rights Reserved
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http:www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package myerr

type Error struct {
	statusCode int
	msg        string
	err        error
}

func (e Error) Error() string {
	return e.msg
}

func (e Error) StatusCode() int {
	return e.statusCode
}
func (e Error) Cause(err error) {
	e.err = err
}

func (e Error) Unwrap() error {
	return e.err
}

func New(msg string) Error {
	return Error{msg: msg}
}

func NewAppendCode(code int, msg string) Error {
	return Error{msg: msg, statusCode: code}
}

func Wrap(msg string, err error) Error {
	return Error{msg: msg, err: err}
}
