#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Print, as JSON, the values an SSG profile gives XCCDF variables.

usage: ssg-values.py DATASTREAM PROFILE_ID VALUE_NAME...

VALUE_NAME is the short name (var_multiple_time_servers). The value is the one
the profile's refine-value selects, or the variable's default.
"""
import json
import sys
import xml.etree.ElementTree as ET

XCCDF = "{http://checklists.nist.gov/xccdf/1.2}"
VALUE_PREFIX = "xccdf_org.ssgproject.content_value_"


def main(datastream, profile_id, names):
    wanted = {VALUE_PREFIX + n: n for n in names}
    selectors, values, found_profile = {}, {}, False
    for _, elem in ET.iterparse(datastream):
        if elem.tag == XCCDF + "Profile":
            if elem.get("id") == profile_id:
                found_profile = True
                for r in elem.iter(XCCDF + "refine-value"):
                    if r.get("idref") in wanted:
                        selectors[r.get("idref")] = r.get("selector")
            elem.clear()
        elif elem.tag == XCCDF + "Value" and elem.get("id") in wanted:
            values[elem.get("id")] = {
                v.get("selector"): (v.text or "") for v in elem.findall(XCCDF + "value")
            }
            elem.clear()
    if not found_profile:
        sys.exit("profile %s not found in %s" % (profile_id, datastream))
    out = {}
    for vid, name in wanted.items():
        if vid not in values:
            sys.exit("variable %s not found in %s" % (name, datastream))
        sel = selectors.get(vid)
        if sel not in values[vid]:
            sel = None
        if sel not in values[vid]:
            sys.exit("variable %s has no value for selector %s" % (name, selectors.get(vid)))
        out[name] = values[vid][sel]
    json.dump(out, sys.stdout)


if __name__ == "__main__":
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2], sys.argv[3:])
