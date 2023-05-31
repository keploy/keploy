#pragma once

#define MAX_DATA 8192
#define PERF_BUFFER_NAME "TLS_DATA_PERF_OUTPUT"

struct TLS_MESSAGE {
  int elapsed;
  int ptid;
  char message[MAX_DATA];
};