/*
 * Copyright 2018- The Pixie Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

#pragma once


#define MAX_DATA_SIZE 8192
enum ssl_data_event_type { kSSLRead, kSSLWrite };

struct ssl_data_event_t {
  enum ssl_data_event_type type;
  uint64_t timestamp_ns;
  uint32_t pid;
  int tid;
  char data[MAX_DATA_SIZE];
  int data_len;
};

struct ssl_data{
  char *buf;
};
