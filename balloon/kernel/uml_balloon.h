/* SPDX-License-Identifier: GPL-2.0 */
/*
 * UML memory balloon core — shared by mconsole mem=± and auto-balloon.
 *
 * Policy constants mirror balloon/src/policy.py in the uml-kernel-build repo.
 */
#ifndef __UML_BALLOON_H__
#define __UML_BALLOON_H__

#include <linux/types.h>

/* Returns number of pages actually unplugged (returned to host). */
unsigned long uml_balloon_unplug_pages(unsigned long n_pages);

/* Returns number of pages actually plugged back into the guest. */
unsigned long uml_balloon_plug_pages(unsigned long n_pages);

/* Pages currently held in the balloon (unplugged). */
unsigned long long uml_balloon_pages(void);

/* True if host supports MADV_REMOVE and balloon is usable. */
bool uml_balloon_available(void);

#endif /* __UML_BALLOON_H__ */
