import {Option} from "@/components/Config/index.d";
import React, {MouseEventHandler} from "react";
import {Box, Button, TextField} from "@mui/material";

import {FormProps, ModalProps} from "@/components/Config/ObjectField/ObjectField";
import {StringField} from "@/components/Config";
const OptionForm = ({ onSubmit, value }: FormProps<Option>) => {

    const [id, setId] = React.useState<string>(value?.id || "")
    const [name, setName] = React.useState<string>(value?.name || "")

    const submitHandler = () => {
        const option = {
            id: id,
            name: name
        }
        onSubmit(option);
    }

    return (
        <>
            <Box my={2}>
                <StringField onChange={setId} name={"ID"} value={id} />
            </Box>
            <Box mb={2}>
                <StringField onChange={setName} name={"Name"} value={name} />
            </Box>
            <Button onClick={submitHandler}>Submit</Button>
        </>
    )
}

export default OptionForm;
