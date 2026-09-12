"""Compare retained backend state without assigning order to storage memberships."""
import copy
import json


def _membership(value):
    if not isinstance(value, str):
        raise ValueError("storage membership must be a string")
    if value == "":
        return []
    members = value.split(",")
    if any(not member or member.strip() != member for member in members) or len(set(members)) != len(members):
        raise ValueError("storage membership is malformed or ambiguous")
    return sorted(members)


def _canonical(value):
    if not isinstance(value, dict) or value.get("active_fault_state_absent") is not True:
        raise ValueError("backend restoration is not established")
    result = copy.deepcopy(value)
    definition = result.get("storage_definition")
    if definition is not None:
        if not isinstance(definition, dict):
            raise ValueError("storage definition is malformed")
        # PVE defines content as allowed types and nodes as applicable nodes.
        # Their membership matters; ordering does not. NFS options are retained
        # verbatim because option order can affect their interpretation.
        for field in ("content", "nodes"):
            if field in definition:
                definition[field] = _membership(definition[field])
    return json.dumps(result, sort_keys=True, separators=(",", ":"), allow_nan=False)


def same_backend_observation(baseline, observed):
    """Require every observed field to match, except membership ordering."""
    try:
        return _canonical(baseline) == _canonical(observed)
    except (TypeError, ValueError):
        return False


def retain_backend_observation(path, baseline, observed, writer):
    """Retain the raw observation before accepting or rejecting restoration."""
    writer(path, observed)
    if not same_backend_observation(baseline, observed):
        raise RuntimeError("Backend restored state differs from preparation; raw observation retained")
