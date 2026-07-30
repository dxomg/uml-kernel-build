// SPDX-License-Identifier: GPL-2.0
/*
 * UML auto-balloon
 *
 * Reuses the classic mconsole mem hotplug primitive (alloc_page +
 * MADV_REMOVE / free_page) and adds a guest-side policy kthread:
 *
 *   allocated = boot_cap - ballooned
 *   usage     = allocated - si_mem_available()
 *   slack     = allocated - usage   (== available)
 *
 *   reclaim (unplug) when slack >= high_slack  → leave reserve_slack
 *   restore (plug)   when slack <= low_slack   → restore toward reserve_slack
 *   never exceed boot_cap; never go below min_allocated
 *
 * Defaults: high=250MiB, reserve=100MiB, low=32MiB, min_allocated=64MiB.
 * Math MUST stay in sync with balloon/src/policy.py (userspace tests).
 */

#include <linux/kernel.h>
#include <linux/mm.h>
#include <linux/module.h>
#include <linux/mutex.h>
#include <linux/kthread.h>
#include <linux/delay.h>
#include <linux/sysfs.h>
#include <linux/kobject.h>
#include <linux/jiffies.h>
#include <linux/math64.h>
#include <init.h>
#include <os.h>
#include <as-layout.h>
#include "mconsole_kern.h"
#include "uml_balloon.h"

/* ---- policy defaults (bytes) — keep in sync with balloon/src/policy.py ---- */
#define UML_BALLOON_HIGH_SLACK		(250ULL * 1024 * 1024)
#define UML_BALLOON_RESERVE_SLACK	(100ULL * 1024 * 1024)
#define UML_BALLOON_LOW_SLACK		(32ULL * 1024 * 1024)
#define UML_BALLOON_MIN_ALLOCATED	(64ULL * 1024 * 1024)
#define UML_BALLOON_STEP		(4ULL * 1024 * 1024)
#define UML_BALLOON_INTERVAL_MS		5000

#define UNPLUGGED_PER_PAGE \
	((PAGE_SIZE - sizeof(struct list_head)) / sizeof(unsigned long))

struct unplugged_pages {
	struct list_head list;
	void *pages[UNPLUGGED_PER_PAGE];
};

static DEFINE_MUTEX(plug_mem_mutex);
static unsigned long long unplugged_pages_count;
static LIST_HEAD(unplugged_pages);
static int unplug_index = UNPLUGGED_PER_PAGE;

static bool balloon_ok;
static struct task_struct *balloon_thread;
static struct kobject *balloon_kobj;

/* sysfs-tunable policy */
static u64 high_slack_bytes = UML_BALLOON_HIGH_SLACK;
static u64 reserve_slack_bytes = UML_BALLOON_RESERVE_SLACK;
static u64 low_slack_bytes = UML_BALLOON_LOW_SLACK;
static u64 min_allocated_bytes = UML_BALLOON_MIN_ALLOCATED;
static u64 step_bytes = UML_BALLOON_STEP;
static u32 interval_ms = UML_BALLOON_INTERVAL_MS;
static u32 enabled = 1;

/* ---------- core plug / unplug (same algorithm as upstream mem_mc) ---------- */

static unsigned long __uml_balloon_unplug_pages(unsigned long n_pages)
{
	unsigned long i, done = 0;

	for (i = 0; i < n_pages; i++) {
		struct unplugged_pages *unplugged;
		struct page *page;
		void *addr;
		int err;

		page = alloc_page(GFP_ATOMIC);
		if (!page)
			break;

		unplugged = page_address(page);
		if (unplug_index == UNPLUGGED_PER_PAGE) {
			/* This page becomes a list node; not dropped to host. */
			list_add(&unplugged->list, &unplugged_pages);
			unplug_index = 0;
		} else {
			struct list_head *entry = unplugged_pages.next;

			addr = unplugged;
			unplugged = list_entry(entry, struct unplugged_pages, list);
			err = os_drop_memory(addr, PAGE_SIZE);
			if (err) {
				pr_err("uml_balloon: MADV_REMOVE failed: %d\n", err);
				__free_page(page);
				break;
			}
			unplugged->pages[unplug_index++] = addr;
		}
		unplugged_pages_count++;
		done++;
	}
	return done;
}

static unsigned long __uml_balloon_plug_pages(unsigned long n_pages)
{
	unsigned long i, done = 0;

	for (i = 0; i < n_pages; i++) {
		struct unplugged_pages *unplugged;
		void *addr;

		if (list_empty(&unplugged_pages))
			break;

		unplugged = list_entry(unplugged_pages.next,
				       struct unplugged_pages, list);
		if (unplug_index > 0) {
			addr = unplugged->pages[--unplug_index];
		} else {
			list_del(&unplugged->list);
			addr = unplugged;
			unplug_index = UNPLUGGED_PER_PAGE;
		}
		free_page((unsigned long)addr);
		unplugged_pages_count--;
		done++;
	}
	return done;
}

unsigned long uml_balloon_unplug_pages(unsigned long n_pages)
{
	unsigned long done;

	if (!balloon_ok || !n_pages)
		return 0;
	mutex_lock(&plug_mem_mutex);
	done = __uml_balloon_unplug_pages(n_pages);
	mutex_unlock(&plug_mem_mutex);
	return done;
}
EXPORT_SYMBOL_GPL(uml_balloon_unplug_pages);

unsigned long uml_balloon_plug_pages(unsigned long n_pages)
{
	unsigned long done;

	if (!balloon_ok || !n_pages)
		return 0;
	mutex_lock(&plug_mem_mutex);
	done = __uml_balloon_plug_pages(n_pages);
	mutex_unlock(&plug_mem_mutex);
	return done;
}
EXPORT_SYMBOL_GPL(uml_balloon_plug_pages);

unsigned long long uml_balloon_pages(void)
{
	unsigned long long n;

	mutex_lock(&plug_mem_mutex);
	n = unplugged_pages_count;
	mutex_unlock(&plug_mem_mutex);
	return n;
}
EXPORT_SYMBOL_GPL(uml_balloon_pages);

bool uml_balloon_available(void)
{
	return balloon_ok;
}
EXPORT_SYMBOL_GPL(uml_balloon_available);

/* ---------- policy helpers (mirror balloon/src/policy.py) ---------- */

static u64 align_down(u64 n, u64 step)
{
	if (!step)
		return n;
	return div64_u64(n, step) * step;
}

static u64 clamp_u64(u64 n, u64 lo, u64 hi)
{
	if (n < lo)
		return lo;
	if (n > hi)
		return hi;
	return n;
}

enum uml_balloon_act {
	UML_BALLOON_NONE = 0,
	UML_BALLOON_UNPLUG,
	UML_BALLOON_PLUG,
};

struct uml_balloon_decision {
	enum uml_balloon_act kind;
	u64 bytes;
	const char *reason;
};

static struct uml_balloon_decision decide(u64 boot_cap, u64 ballooned, u64 available)
{
	struct uml_balloon_decision d = { .kind = UML_BALLOON_NONE, .bytes = 0,
					  .reason = "within_hysteresis" };
	u64 allocated, usage, slack, target, delta, max_drop;

	if (!enabled) {
		d.reason = "disabled";
		return d;
	}

	if (ballooned > boot_cap)
		ballooned = boot_cap;
	allocated = boot_cap - ballooned;
	if (available > allocated)
		available = allocated;
	usage = allocated - available;
	slack = allocated - usage; /* == available */

	if (slack >= high_slack_bytes) {
		target = usage + reserve_slack_bytes;
		target = clamp_u64(target, min_allocated_bytes, boot_cap);
		if (target >= allocated) {
			d.reason = "reclaim_skipped_target_ge_allocated";
			return d;
		}
		delta = align_down(allocated - target, step_bytes);
		max_drop = (allocated > min_allocated_bytes) ?
			   allocated - min_allocated_bytes : 0;
		delta = min_t(u64, delta, align_down(max_drop, step_bytes));
		if (delta < step_bytes) {
			d.reason = "reclaim_below_step";
			return d;
		}
		d.kind = UML_BALLOON_UNPLUG;
		d.bytes = delta;
		d.reason = "reclaim_high_slack";
		return d;
	}

	if (slack <= low_slack_bytes && ballooned > 0) {
		target = usage + reserve_slack_bytes;
		target = clamp_u64(target, min_allocated_bytes, boot_cap);
		if (target <= allocated) {
			d.reason = "restore_skipped_target_le_allocated";
			return d;
		}
		delta = align_down(target - allocated, step_bytes);
		delta = min_t(u64, delta, ballooned);
		delta = align_down(delta, step_bytes);
		if (delta < step_bytes) {
			d.reason = "restore_below_step";
			return d;
		}
		d.kind = UML_BALLOON_PLUG;
		d.bytes = delta;
		d.reason = "restore_low_slack";
		return d;
	}

	return d;
}

/* ---------- kthread ---------- */

static int balloon_worker(void *data)
{
	while (!kthread_should_stop()) {
		u64 boot_cap, ballooned, available, pages;
		struct uml_balloon_decision d;
		unsigned long n;

		if (enabled && balloon_ok) {
			boot_cap = (u64)physmem_size;
			mutex_lock(&plug_mem_mutex);
			ballooned = unplugged_pages_count * PAGE_SIZE;
			mutex_unlock(&plug_mem_mutex);
			available = (u64)si_mem_available() << PAGE_SHIFT;

			d = decide(boot_cap, ballooned, available);
			if (d.kind != UML_BALLOON_NONE && d.bytes >= PAGE_SIZE) {
				pages = div64_u64(d.bytes, PAGE_SIZE);
				if (d.kind == UML_BALLOON_UNPLUG) {
					n = uml_balloon_unplug_pages(pages);
					if (n)
						pr_info_ratelimited(
							"uml_balloon: unplug %lu pages (%s)\n",
							n, d.reason);
				} else {
					n = uml_balloon_plug_pages(pages);
					if (n)
						pr_info_ratelimited(
							"uml_balloon: plug %lu pages (%s)\n",
							n, d.reason);
				}
			}
		}
		msleep_interruptible(interval_ms ? interval_ms : UML_BALLOON_INTERVAL_MS);
	}
	return 0;
}

/* ---------- sysfs ---------- */

#define BALLOON_ATTR_RW(_name) \
static ssize_t _name##_show(struct kobject *k, struct kobj_attribute *a, char *buf) \
{ return sysfs_emit(buf, "%llu\n", (unsigned long long)_name); } \
static ssize_t _name##_store(struct kobject *k, struct kobj_attribute *a, \
			     const char *buf, size_t count) \
{ \
	unsigned long long v; \
	int err = kstrtoull(buf, 0, &v); \
	if (err) return err; \
	_name = v; \
	return count; \
} \
static struct kobj_attribute _name##_attr = __ATTR_RW(_name)

BALLOON_ATTR_RW(high_slack_bytes);
BALLOON_ATTR_RW(reserve_slack_bytes);
BALLOON_ATTR_RW(low_slack_bytes);
BALLOON_ATTR_RW(min_allocated_bytes);
BALLOON_ATTR_RW(step_bytes);

static ssize_t interval_ms_show(struct kobject *k, struct kobj_attribute *a, char *buf)
{
	return sysfs_emit(buf, "%u\n", interval_ms);
}
static ssize_t interval_ms_store(struct kobject *k, struct kobj_attribute *a,
				 const char *buf, size_t count)
{
	unsigned int v;
	int err = kstrtouint(buf, 0, &v);

	if (err)
		return err;
	if (v < 100)
		v = 100;
	interval_ms = v;
	return count;
}
static struct kobj_attribute interval_ms_attr = __ATTR_RW(interval_ms);

static ssize_t enabled_show(struct kobject *k, struct kobj_attribute *a, char *buf)
{
	return sysfs_emit(buf, "%u\n", enabled);
}
static ssize_t enabled_store(struct kobject *k, struct kobj_attribute *a,
			     const char *buf, size_t count)
{
	unsigned int v;
	int err = kstrtouint(buf, 0, &v);

	if (err)
		return err;
	enabled = v ? 1 : 0;
	return count;
}
static struct kobj_attribute enabled_attr = __ATTR_RW(enabled);

static ssize_t boot_cap_bytes_show(struct kobject *k, struct kobj_attribute *a, char *buf)
{
	return sysfs_emit(buf, "%llu\n", (unsigned long long)physmem_size);
}
static struct kobj_attribute boot_cap_bytes_attr = __ATTR_RO(boot_cap_bytes);

static ssize_t ballooned_bytes_show(struct kobject *k, struct kobj_attribute *a, char *buf)
{
	return sysfs_emit(buf, "%llu\n", uml_balloon_pages() * PAGE_SIZE);
}
static struct kobj_attribute ballooned_bytes_attr = __ATTR_RO(ballooned_bytes);

static ssize_t allocated_bytes_show(struct kobject *k, struct kobj_attribute *a, char *buf)
{
	u64 b = uml_balloon_pages() * PAGE_SIZE;
	u64 cap = physmem_size;

	return sysfs_emit(buf, "%llu\n", cap > b ? cap - b : 0);
}
static struct kobj_attribute allocated_bytes_attr = __ATTR_RO(allocated_bytes);

static ssize_t available_bytes_show(struct kobject *k, struct kobj_attribute *a, char *buf)
{
	return sysfs_emit(buf, "%llu\n",
			  (unsigned long long)si_mem_available() << PAGE_SHIFT);
}
static struct kobj_attribute available_bytes_attr = __ATTR_RO(available_bytes);

static ssize_t usage_bytes_show(struct kobject *k, struct kobj_attribute *a, char *buf)
{
	u64 cap = physmem_size;
	u64 b = uml_balloon_pages() * PAGE_SIZE;
	u64 allocated = cap > b ? cap - b : 0;
	u64 avail = (u64)si_mem_available() << PAGE_SHIFT;

	if (avail > allocated)
		avail = allocated;
	return sysfs_emit(buf, "%llu\n", allocated - avail);
}
static struct kobj_attribute usage_bytes_attr = __ATTR_RO(usage_bytes);

static ssize_t slack_bytes_show(struct kobject *k, struct kobj_attribute *a, char *buf)
{
	u64 cap = physmem_size;
	u64 b = uml_balloon_pages() * PAGE_SIZE;
	u64 allocated = cap > b ? cap - b : 0;
	u64 avail = (u64)si_mem_available() << PAGE_SHIFT;

	if (avail > allocated)
		avail = allocated;
	return sysfs_emit(buf, "%llu\n", avail);
}
static struct kobj_attribute slack_bytes_attr = __ATTR_RO(slack_bytes);

static struct attribute *balloon_attrs[] = {
	&enabled_attr.attr,
	&high_slack_bytes_attr.attr,
	&reserve_slack_bytes_attr.attr,
	&low_slack_bytes_attr.attr,
	&min_allocated_bytes_attr.attr,
	&step_bytes_attr.attr,
	&interval_ms_attr.attr,
	&boot_cap_bytes_attr.attr,
	&ballooned_bytes_attr.attr,
	&allocated_bytes_attr.attr,
	&available_bytes_attr.attr,
	&usage_bytes_attr.attr,
	&slack_bytes_attr.attr,
	NULL,
};

static const struct attribute_group balloon_grp = {
	.attrs = balloon_attrs,
};

/* ---------- mconsole mem device (thin wrapper, same UX) ---------- */

static int mem_config(char *str, char **error_out)
{
	unsigned long long diff;
	char *ret;
	int add;
	unsigned long n, did;

	if (!balloon_ok) {
		*error_out = "Balloon not available on this host";
		return -ENOSYS;
	}
	if (str[0] != '=') {
		*error_out = "Expected '=' after 'mem'";
		return -EINVAL;
	}
	str++;
	if (str[0] == '-')
		add = 0;
	else if (str[0] == '+')
		add = 1;
	else {
		*error_out = "Expected increment to start with '-' or '+'";
		return -EINVAL;
	}
	str++;
	diff = memparse(str, &ret);
	if (*ret != '\0') {
		*error_out = "Failed to parse memory increment";
		return -EINVAL;
	}
	n = (unsigned long)(diff / PAGE_SIZE);
	if (add)
		did = uml_balloon_plug_pages(n);
	else
		did = uml_balloon_unplug_pages(n);
	if (!did && n) {
		*error_out = add ? "Nothing to plug" : "Failed to unplug";
		return -ENOMEM;
	}
	return 0;
}

static int mem_get_config(char *name, char *str, int size, char **error_out)
{
	char buf[32];
	int len = 0;

	sprintf(buf, "%ld", uml_physmem);
	CONFIG_CHUNK(str, size, len, buf, 1);
	return len;
}

static int mem_id(char **str, int *start_out, int *end_out)
{
	*start_out = 0;
	*end_out = 0;
	return 0;
}

static int mem_remove(int n, char **error_out)
{
	*error_out = "Memory doesn't support the remove operation";
	return -EBUSY;
}

static struct mc_device mem_mc = {
	.list		= LIST_HEAD_INIT(mem_mc.list),
	.name		= "mem",
	.config		= mem_config,
	.get_config	= mem_get_config,
	.id		= mem_id,
	.remove		= mem_remove,
};

/* ---------- init ---------- */

static int __init uml_balloon_init(void)
{
	int err;

	if (!can_drop_memory()) {
		pr_err("uml_balloon: host lacks MADV_REMOVE — disabled\n");
		balloon_ok = false;
		return 0;
	}
	balloon_ok = true;

	mconsole_register_dev(&mem_mc);

	balloon_kobj = kobject_create_and_add("uml_balloon", kernel_kobj);
	if (!balloon_kobj) {
		pr_warn("uml_balloon: sysfs kobject failed\n");
	} else {
		err = sysfs_create_group(balloon_kobj, &balloon_grp);
		if (err)
			pr_warn("uml_balloon: sysfs group failed: %d\n", err);
	}

	balloon_thread = kthread_run(balloon_worker, NULL, "uml-balloon");
	if (IS_ERR(balloon_thread)) {
		pr_warn("uml_balloon: kthread failed: %ld\n", PTR_ERR(balloon_thread));
		balloon_thread = NULL;
	} else {
		pr_info("uml_balloon: auto-balloon enabled (high=%lluM reserve=%lluM low=%lluM)\n",
			high_slack_bytes >> 20, reserve_slack_bytes >> 20,
			low_slack_bytes >> 20);
	}
	return 0;
}
__initcall(uml_balloon_init);
