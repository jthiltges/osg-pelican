

export const stringSort = (a: string, b: string) => {
    return a.localeCompare(b)
}

export const createId = (name: string) => {
    return name.toLowerCase().split(".").join("-");
}

const buildPatchHelper = (keys: string[], value: any) : any => {
    if( keys.length === 1 ) {
        return {[keys[0]]: value}
    }
    return {[keys[0]]: buildPatchHelper(keys.slice(1), value)}
}

export const buildPatch = (name: string, value: any) => {
    return buildPatchHelper(name.split("."), value)
}

export const submitConfigChange = async (data: any) => {

    const response = await fetch('/api/v1.0/config', {
        method: 'PATCH',
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify(data)
    })

    if( response.ok ) {
        return response.json()
    }

    // Throw the best possible error message
    let responseData = undefined;
    try {
        responseData = await response.json()
    } catch {
        throw new Error(`Failed to path config: Status ${response.status}`)
    }

    // Throw the server provided error
    throw new Error(`${response.status}: ${responseData['error']}`)
}
