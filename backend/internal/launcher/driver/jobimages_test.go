// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

// sameJobImage is a driver's job images when one image stands in for both engines' job images.
func sameJobImage(image string) map[string]string {
	return map[string]string{"tofu": image, "ansible": image}
}
