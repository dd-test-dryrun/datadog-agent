#ifndef __REDIS_DECODING_H
#define __REDIS_DECODING_H

#include "protocols/redis/decoding-maps.h"
#include "protocols/helpers/pktbuf.h"

#define BLK_SIZE (16)
PKTBUF_READ_INTO_BUFFER(redis_bulk, MAX_KEY_LEN, BLK_SIZE)

// Read a CRLF terminator from the packet buffer. The terminator is expected to be in the format: \r\n.
// The function returns true if the terminator was successfully read, or false if the terminator could not be read.
static __always_inline bool read_crlf(pktbuf_t pkt) {
    char terminator[RESP_FIELD_TERMINATOR_LEN];
    if (pktbuf_load_bytes_from_current_offset(pkt, terminator, RESP_FIELD_TERMINATOR_LEN) < 0) {
        return false;
    }
    pktbuf_advance(pkt, RESP_FIELD_TERMINATOR_LEN);
    return terminator[0] == RESP_TERMINATOR_1 && terminator[1] == RESP_TERMINATOR_2;
}

// Read an array message from the packet buffer. The array message is expected to be in the format:
// *<param_count>\r\n<param1>\r\n<param2>\r\n...
// where <param_count> is the number of parameters in the array, and <param1>, <param2>, etc. are the parameters themselves.
// The function returns the number of parameters in the array, or 0 if the array message could not be read.
static __always_inline u32 read_array_message(pktbuf_t pkt) {
    // Verify RESP array prefix
    char first_byte;
    if (pktbuf_load_bytes_from_current_offset(pkt, &first_byte, sizeof(first_byte)) < 0 || first_byte != RESP_ARRAY_PREFIX) {
        return 0;
    }
    pktbuf_advance(pkt, sizeof(first_byte));

    // Read parameter count
    char param_count;
    // Assuming single-digit param count, as currently we don't need more.
    if (pktbuf_load_bytes_from_current_offset(pkt, &param_count, 1) < 0) {
        return 0;
    }
    pktbuf_advance(pkt, sizeof(param_count));

    if (param_count < '0' || param_count > '9') {
        return 0;
    }

    if (!read_crlf(pkt)) {
        return 0;
    }

    return param_count - '0';
}

static __always_inline u16 get_key_len(pktbuf_t pkt) {
    u32 current_offset = pktbuf_data_offset(pkt);
    const u32 data_end = pktbuf_data_end(pkt);

    char bulk_prefix;
    // Verify we can read the RESP bulk prefix.
    if (current_offset + sizeof(bulk_prefix) > data_end) {
        return 0;
    }
    if (pktbuf_load_bytes(pkt, current_offset, &bulk_prefix, sizeof(bulk_prefix)) < 0 || bulk_prefix != RESP_BULK_PREFIX) {
        return 0;
    }
    current_offset++;

    // Read key length (up to 3 digits)
    char key_size_bytes[3] = {};
    if (current_offset + sizeof(key_size_bytes) > data_end) {
        return 0;
    }
    if (pktbuf_load_bytes(pkt, current_offset, key_size_bytes, sizeof(key_size_bytes)) < 0) {
        return 0;
    }

    u16 key_size = 0;
    u32 digits_read = 0;
    // The key length is a decimal number, so we need to convert it from ASCII to an integer.
    #pragma unroll (3)
    for (int i = 0; i < 3; i++) {
        if (key_size_bytes[i] == RESP_TERMINATOR_1) {
            break;
        }
        if (key_size_bytes[i] < '0' || key_size_bytes[i] > '9') {
            return 0;
        }
        key_size = key_size * 10 + (key_size_bytes[i] - '0');
        digits_read++;
    }

    // Advance past the digits we read
    current_offset += digits_read;
    pktbuf_set_offset(pkt, current_offset);

    if (!read_crlf(pkt)) {
        return 0;
    }

    if (key_size <= 0 || key_size > 999) {
        return 0;
    }

    return key_size;
}

static __always_inline bool read_key_name(pktbuf_t pkt, char *buf, u8 buf_len, u16 *out_key_len, bool *truncated) {
    const u32 key_size = *out_key_len > MAX_KEY_LEN - 1 ? MAX_KEY_LEN - 1 : *out_key_len;
    const u32 final_key_size = key_size > buf_len ? buf_len : key_size;
    if (final_key_size == 0) {
        return false;
    }

    pktbuf_read_into_buffer_redis_bulk(buf, pkt, pktbuf_data_offset(pkt));
    pktbuf_advance(pkt, *out_key_len);
    
    // Read and skip past the CRLF after the key data
    if (!read_crlf(pkt)) {
        return false;
    }

    *truncated = final_key_size < *out_key_len;
    *out_key_len = final_key_size;
    return true;
}

// Process a Redis request from the packet buffer. The function reads the request from the packet buffer,
// and returns the method (GET or SET) and the key(up to MAX_KEY_LEN bytes).
static __always_inline void process_redis_request(pktbuf_t pkt, conn_tuple_t *conn_tuple) {
    u32 param_count = read_array_message(pkt);
    if (param_count == 0) {
        return;
    }
    // GET message has 2 parameters, SET message has 3-5 parameters. Anything else is irrelevant for us.
    if (param_count < 2 || param_count > 5) {
        return;
    }

    const u16 method_len = get_key_len(pkt);
    char method[METHOD_LEN + 1] = {};
    if (method_len <= 0 || method_len > METHOD_LEN) {
        return;
    }
    
    if (pktbuf_load_bytes_from_current_offset(pkt, method, METHOD_LEN) < 0) {
        return;
    }
    pktbuf_advance(pkt, method_len);
    
    // Read CRLF after method
    if (!read_crlf(pkt)) {
        return;
    }

    redis_transaction_t transaction = {};
    transaction.request_started = bpf_ktime_get_ns();
    if (bpf_memcmp(method, REDIS_CMD_SET, METHOD_LEN) == 0) {
        transaction.command = REDIS_SET;
    } else if (bpf_memcmp(method, REDIS_CMD_GET, METHOD_LEN) == 0) {
        transaction.command = REDIS_GET;
    } else {
        return;
    }

    // Now read the key length
    transaction.buf_len = get_key_len(pkt);
    if (transaction.buf_len == 0) {
        return;
    }

    if (!read_key_name(pkt, transaction.buf, sizeof(transaction.buf), &transaction.buf_len, &transaction.truncated)) {
        return;
    }

    bpf_map_update_elem(&redis_in_flight, conn_tuple, &transaction, BPF_ANY);
}

// Handles a TCP termination event by deleting the connection tuple from the in-flight map.
static void __always_inline redis_tcp_termination(conn_tuple_t *tup) {
    bpf_map_delete_elem(&redis_in_flight, tup);
    flip_tuple(tup);
    bpf_map_delete_elem(&redis_in_flight, tup);
}

// Enqueues a batch of events to the user-space. To spare stack size, we take a scratch buffer from the map, copy
// the connection tuple and the transaction to it, and then enqueue the event.
static __always_inline void redis_batch_enqueue_wrapper(conn_tuple_t *tuple, redis_transaction_t *tx) {
    u32 zero = 0;
    redis_event_t *event = bpf_map_lookup_elem(&redis_scratch_buffer, &zero);
    if (!event) {
        return;
    }

    bpf_memcpy(&event->tuple, tuple, sizeof(conn_tuple_t));
    bpf_memcpy(&event->tx, tx, sizeof(redis_transaction_t));
    redis_batch_enqueue(event);
}

static void __always_inline process_redis_response(pktbuf_t pkt, conn_tuple_t *tup, redis_transaction_t *transaction) {
    char first_byte;
    if (pktbuf_load_bytes_from_current_offset(pkt, &first_byte, sizeof(first_byte)) < 0) {
        return;
    }
    if (first_byte == RESP_ERROR_PREFIX) {
        transaction->is_error = true;
        goto enqueue;
    }
    if (transaction->command == REDIS_GET) {
        if (first_byte != RESP_BULK_PREFIX) {
            goto cleanup;
        }
        goto enqueue;
    } else{
        if (first_byte != RESP_SIMPLE_STRING_PREFIX) {
            goto cleanup;
        }
        goto enqueue;
    }

enqueue:
    transaction->response_last_seen = bpf_ktime_get_ns();
    redis_batch_enqueue_wrapper(tup, transaction);
cleanup:
    bpf_map_delete_elem(&redis_in_flight, tup);
}

SEC("socket/redis_process")
int socket__redis_process(struct __sk_buff *skb) {
    skb_info_t skb_info = {};
    conn_tuple_t conn_tuple = {};
    if (!fetch_dispatching_arguments(&conn_tuple, &skb_info)) {
        return 0;
    }

    if (is_tcp_termination(&skb_info)) {
        redis_tcp_termination(&conn_tuple);
        return 0;
    }
    normalize_tuple(&conn_tuple);
    pktbuf_t pkt = pktbuf_from_skb(skb, &skb_info);

    redis_transaction_t *transaction = bpf_map_lookup_elem(&redis_in_flight, &conn_tuple);
    if (transaction == NULL) {
        process_redis_request(pkt, &conn_tuple);
    } else {
        process_redis_response(pkt, &conn_tuple, transaction);
    }

    return 0;
}

SEC("uprobe/redis_tls_process")
int uprobe__redis_tls_process(struct pt_regs *ctx) {
    const __u32 zero = 0;

    tls_dispatcher_arguments_t *args = bpf_map_lookup_elem(&tls_dispatcher_arguments, &zero);
    if (args == NULL) {
        return 0;
    }

    // Copying the tuple to the stack to handle verifier issues on kernel 4.14.
    conn_tuple_t tup = args->tup;

    pktbuf_t pkt = pktbuf_from_tls(ctx, args);
    redis_transaction_t *transaction = bpf_map_lookup_elem(&redis_in_flight, &tup);
    if (transaction == NULL) {
        process_redis_request(pkt, &tup);
    } else {
        process_redis_response(pkt, &tup, transaction);
    }
    return 0;
}

SEC("uprobe/redis_tls_termination")
int uprobe__redis_tls_termination(struct pt_regs *ctx) {
    const __u32 zero = 0;

    tls_dispatcher_arguments_t *args = bpf_map_lookup_elem(&tls_dispatcher_arguments, &zero);
    if (args == NULL) {
        return 0;
    }

    // Copying the tuple to the stack to handle verifier issues on kernel 4.14.
    conn_tuple_t tup = args->tup;
    redis_tcp_termination(&tup);

    return 0;
}

#endif /* __REDIS_DECODING_H */
