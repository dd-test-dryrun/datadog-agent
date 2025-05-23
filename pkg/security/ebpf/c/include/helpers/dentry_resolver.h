#ifndef _HELPERS_DENTRY_RESOLVER_H_
#define _HELPERS_DENTRY_RESOLVER_H_

#include "constants/custom.h"
#include "maps.h"

#include "buffer_selector.h"

int __attribute__((always_inline)) tail_call_dr_progs(void *ctx, enum TAIL_CALL_PROG_TYPE prog_type, int key) {
    switch (prog_type) {
    case KPROBE_OR_FENTRY_TYPE:
        bpf_tail_call_compat(ctx, &dentry_resolver_kprobe_or_fentry_progs, key);
        break;
    case TRACEPOINT_TYPE:
        bpf_tail_call_compat(ctx, &dentry_resolver_tracepoint_progs, key);
        break;
    }
    return 0;
}

int __attribute__((always_inline)) resolve_dentry(void *ctx, enum TAIL_CALL_PROG_TYPE prog_type) {
    return tail_call_dr_progs(ctx, prog_type, DR_AD_FILTER_KEY);
}

int __attribute__((always_inline)) resolve_dentry_no_syscall(void *ctx, enum TAIL_CALL_PROG_TYPE prog_type) {
    return tail_call_dr_progs(ctx, prog_type, DR_DENTRY_RESOLVER_KERN_INPUTS);
}

int __attribute__((always_inline)) monitor_resolution_err(u32 resolution_err) {
    if (resolution_err > 0) {
        struct bpf_map_def *erpc_stats = select_buffer(&dr_erpc_stats_fb, &dr_erpc_stats_bb, ERPC_MONITOR_KEY);
        if (erpc_stats == NULL) {
            return 0;
        }

        struct dr_erpc_stats_t *stats = bpf_map_lookup_elem(erpc_stats, &resolution_err);
        if (stats == NULL) {
            return 0;
        }
        __sync_fetch_and_add(&stats->count, 1);
    }
    return 0;
}

u32 __attribute__((always_inline)) parse_erpc_request(struct dr_erpc_state_t *state, void *data) {
    u32 err = 0;
    int ret = bpf_probe_read(&state->key, sizeof(state->key), data);
    if (ret < 0) {
        err = DR_ERPC_READ_PAGE_FAULT;
        goto exit;
    }
    ret = bpf_probe_read(&state->userspace_buffer, sizeof(state->userspace_buffer), data + sizeof(state->key));
    if (ret < 0) {
        err = DR_ERPC_READ_PAGE_FAULT;
        goto exit;
    }
    ret = bpf_probe_read(&state->buffer_size, sizeof(state->buffer_size), data + sizeof(state->key) + sizeof(state->userspace_buffer));
    if (ret < 0) {
        err = DR_ERPC_READ_PAGE_FAULT;
        goto exit;
    }
    ret = bpf_probe_read(&state->challenge, sizeof(state->challenge), data + sizeof(state->key) + sizeof(state->userspace_buffer) + sizeof(state->buffer_size));
    if (ret < 0) {
        err = DR_ERPC_READ_PAGE_FAULT;
        goto exit;
    }

    state->iteration = 0;
    state->ret = 0;
    state->cursor = 0;

exit:
    return err;
}

int __attribute__((always_inline)) handle_dr_request(ctx_t *ctx, void *data, u32 dr_erpc_key) {
    u32 key = 0;
    struct dr_erpc_state_t *state = bpf_map_lookup_elem(&dr_erpc_state, &key);
    if (state == NULL) {
        return 0;
    }

    u32 resolution_err = parse_erpc_request(state, data);
    if (resolution_err > 0) {
        goto exit;
    }

    tail_call_dr_progs(ctx, KPROBE_OR_FENTRY_TYPE, dr_erpc_key);

exit:
    monitor_resolution_err(resolution_err);
    return 0;
}

int __attribute__((always_inline)) select_dr_key(enum TAIL_CALL_PROG_TYPE prog_type, int kprobe_key, int tracepoint_key) {
    switch (prog_type) {
    case KPROBE_OR_FENTRY_TYPE:
        return kprobe_key;
    default: // TRACEPOINT_TYPE
        return tracepoint_key;
    }
}

// cache_syscall checks the event policy in order to see if the syscall struct can be cached
void __attribute__((always_inline)) cache_dentry_resolver_input(struct dentry_resolver_input_t *input) {
    u64 pid_tgid = bpf_get_current_pid_tgid();
    bpf_map_update_elem(&dentry_resolver_inputs, &pid_tgid, input, BPF_ANY);
}

#endif
