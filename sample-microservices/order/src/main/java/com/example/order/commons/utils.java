package com.example.order.commons;

import java.io.File;
import java.io.FileWriter;
import java.io.FilenameFilter;
import java.io.IOException;
import java.net.URI;
import java.net.URISyntaxException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.ArrayList;
import java.util.List;

import com.example.order.models.Contract;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;

public class utils {
    // function that take response and request and create the interaction and the contract
    public static void createContract(com.example.order.models.Response response, com.example.order.models.Request request,String consumer,String provider) {
        // create the interaction
        com.example.order.models.Interaction interaction = new com.example.order.models.Interaction();
        interaction.setRequest(request);
        interaction.setResponse(response);
        interaction.setDescription("test interaction description");
        //check if the contract exists or not
        if(checkContractExist(consumer, provider)){
            com.example.order.models.Contract contract = loadContract(consumer, provider);
            contract.getInteractions().add(interaction);
            saveContract(contract);

        }
        else{
            // if the contract does not exist, create a new contract and add the interaction to it
            com.example.order.models.Contract contract = new com.example.order.models.Contract(); 
            contract.setConsumer(new com.example.order.models.Participant(consumer));
            contract.setProvider(new com.example.order.models.Participant(provider));
            List<com.example.order.models.Interaction> interactions = new ArrayList<com.example.order.models.Interaction>();
            interactions.add(interaction);
            contract.setInteractions(interactions);
            // save the contract
            saveContract(contract);
        }
        


    }
    public static Boolean checkContractExist(String consumer, String provider) {
        File file = new File("./"+consumer+"_"+provider+".json");
        return file.exists();
    }

    public static String extractProviderHostPort(String url) {
        URI uri;
        try {
            uri = new URI(url.toString());
        } catch (URISyntaxException e) {
            // Handle the exception according to your needs
            e.printStackTrace();
            return null;
        }
         // Get the port from the URI
         int port = uri.getPort();
        return Integer.toString(port);
    }
    public static Boolean saveContract(com.example.order.models.Contract contract) {
        // Create an ObjectMapper to save class into it
        ObjectMapper objectMapper = new ObjectMapper();
        objectMapper.enable(SerializationFeature.INDENT_OUTPUT);

        try {
            // Convert the Contract object to a JSON string
            String contractJson = objectMapper.writeValueAsString(contract);
            // Write the JSON string to a file using try-with-resources
            try (FileWriter fileWriter = new FileWriter("./" + contract.getConsumer().getName() + "_" + contract.getProvider().getName() + ".json")) {
                fileWriter.write(contractJson);
            }   
        
        } catch (IOException e) {
            // Handle the exception according to your needs
            e.printStackTrace();
            return false;
        }
        return true;

    }
    //function to load the contract
    public static com.example.order.models.Contract loadContract(String consumer, String provider) {
        // Create an ObjectMapper to read the class from the file
        ObjectMapper objectMapper = new ObjectMapper();
        com.example.order.models.Contract contract = null;
        try{
            String workingDirectory = System.getProperty("user.dir");
            System.out.println("Current working directory: " + workingDirectory);
            File jsonFile = new File("/home/ahmed/Desktop/GSOC/Keploy/Issues/issue1541/8085_8084.json");
            
            if (jsonFile.exists()) {
                // Read the content of the file and deserialize it into Contract class
                contract = objectMapper.readValue(jsonFile, com.example.order.models.Contract.class);
                System.out.println("Contract loaded successfully from: " + jsonFile.getAbsolutePath());
            } else {
                System.out.println("File not found: " + jsonFile.getAbsolutePath());
            }
        }
        catch (IOException e) {
            // Handle the exception according to your needs
           e.printStackTrace();
        }
        return contract;
    }
    //function for consumer to test using contract
    public static com.example.order.models.Response test(String consumer,String url,String method){
        // load the contract
        com.example.order.models.Contract contract = loadContract(consumer,"");
        // get the interactions
        List<com.example.order.models.Interaction> interactions = contract.getInteractions();
        // loop through the interactions
        com.example.order.models.Response actualResponse = null;
        for (com.example.order.models.Interaction interaction : interactions) {
            // get the request
            com.example.order.models.Request request = interaction.getRequest();
            // get the response
            com.example.order.models.Response response = interaction.getResponse();
            // log the request and response
            System.out.println("Request: " + request);
            System.out.println("Response: " + response);
            if (request.getURL().equals(url)&& request.getMethod().equals(method)){
                actualResponse = response;
                break;
            }
        }
        return actualResponse;
    }
    //function for provider to verify the contract
    public static Boolean verify(String provider){
        // load the contract
        com.example.order.models.Contract contract = loadContract("",provider);
        // get the interactions
        List<com.example.order.models.Interaction> interactions = contract.getInteractions();
        // loop through the interactions
        for (com.example.order.models.Interaction interaction : interactions) {
            // get the request
            com.example.order.models.Request request = interaction.getRequest();
            // get the response
            com.example.order.models.Response response = interaction.getResponse();
            // log the request and response
            System.out.println("Request: " + request);
            System.out.println("Response: " + response);
            // make the request and compare the response
            // go implement this function
            // makeRequest(request);

            // if the response is not the same as the contract, return false
            // else return true
        }
        return true;
    }
    //function to make the request
    public static void makeRequest(com.example.order.models.Request request){
        // go implement this function
        
    }


}
